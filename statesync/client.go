// (c) 2021-2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package statesync

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/ava-labs/avalanchego/ids"

	"github.com/ava-labs/coreth/ethdb/memorydb"

	"github.com/ava-labs/avalanchego/codec"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/version"

	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/ethdb"
	"github.com/ava-labs/coreth/peer"
	"github.com/ava-labs/coreth/plugin/evm/message"
	"github.com/ava-labs/coreth/trie"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

const DefaultMaxRetryDelay = 10 * time.Second

var (
	StateSyncVersion      = version.NewDefaultApplication(constants.PlatformName, 1, 7, 7)
	errEmptyResponse      = errors.New("empty response")
	errTooManyBlocks      = errors.New("response contains more blocks than requested")
	errHashMismatch       = errors.New("hash does not match expected value")
	errInvalidRangeProof  = errors.New("failed to verify range proof")
	errExceededRetryLimit = errors.New("exceeded request retry limit")
	errTooManyLeaves      = errors.New("response contains more than requested leaves")
	errUnmarshalResponse  = errors.New("failed to unmarshal response")
)
var _ Client = &client{}

// Client is a state sync client that synchronously fetches data from the network
type Client interface {
	// GetLeafs synchronously sends given request, returning parsed *LeafsResponse or error
	GetLeafs(request message.LeafsRequest) (*message.LeafsResponse, error)

	// GetBlocks synchronously retrieves blocks starting with specified common.Hash and height up to specified parents
	// specified range from height to height-parents is inclusive
	GetBlocks(blockHash common.Hash, height uint64, parents uint16) ([]*types.Block, error)

	// GetCode synchronously retrieves code associated with given common.Hash
	GetCode(common.Hash) ([]byte, error)
}

// parseResponseFn parses given response bytes in context of specified request
// Validates response in context of the request
// Ensures that the returned interface matches the expected response type of the request
type parseResponseFn func(request message.Request, response []byte) (interface{}, error)

type client struct {
	networkClient  peer.Client
	codec          codec.Manager
	maxAttempts    uint8
	maxRetryDelay  time.Duration
	stateSyncNodes []ids.ShortID
	nodeIdx        int
}

func NewClient(networkClient peer.Client, maxAttempts uint8, maxRetryDelay time.Duration, codec codec.Manager, stateSyncNodes []ids.ShortID) *client {
	return &client{
		networkClient:  networkClient,
		maxAttempts:    maxAttempts,
		maxRetryDelay:  maxRetryDelay,
		codec:          codec,
		stateSyncNodes: stateSyncNodes,
	}
}

// GetLeafs synchronously retrieves leafs as per given [message.LeafsRequest]
// Retries when:
// - response bytes could not be unmarshalled to [message.LeafsResponse]
// - response keys do not correspond to the requested range.
// - response does not contain a valid merkle proof.
// Returns error if retries have been exceeded
func (c *client) GetLeafs(req message.LeafsRequest) (*message.LeafsResponse, error) {
	data, err := c.get(req, c.maxAttempts, c.maxRetryDelay, c.parseLeafsResponse)
	if err != nil {
		return nil, err
	}

	response, ok := data.(message.LeafsResponse)
	if !ok {
		return nil, fmt.Errorf("received unexpected type in response, expected: %T", response)
	}

	return &response, err
}

// parseLeafsResponse validates given object as message.LeafsResponse
// assumes reqIntf is of type message.LeafsRequest
// returns a non-nil error if the request should be retried
// returns error when:
// - response bytes could not be marshalled into message.LeafsResponse
// - number of response keys is not equal to the response values
// - first and last key in the response is not within the requested start and end range
// - response keys are not in increasing order
// - proof validation failed
func (c *client) parseLeafsResponse(reqIntf message.Request, data []byte) (interface{}, error) {
	var leafsResponse message.LeafsResponse
	if _, err := c.codec.Unmarshal(data, &leafsResponse); err != nil {
		return nil, err
	}

	leafsRequest := reqIntf.(message.LeafsRequest)

	// Ensure that the response does not contain more than the maximum requested number of leaves.
	if len(leafsResponse.Keys) > int(leafsRequest.Limit) || len(leafsResponse.Vals) > int(leafsRequest.Limit) {
		return nil, fmt.Errorf("%w: (%d) > %d)", errTooManyLeaves, len(leafsResponse.Keys), leafsRequest.Limit)
	}

	// An empty response (no more keys) requires a merkle proof
	if len(leafsResponse.Keys) == 0 && len(leafsResponse.ProofKeys) == 0 {
		return nil, fmt.Errorf("empty key response must include merkle proof")
	}

	var proof ethdb.Database
	// Populate proof when ProofKeys are present in the response. Its ok to pass it as nil to the trie.VerifyRangeProof
	// function as it will assert that all the leaves belonging to the specified root are present.
	if len(leafsResponse.ProofKeys) > 0 {
		if len(leafsResponse.ProofKeys) != len(leafsResponse.ProofVals) {
			return nil, fmt.Errorf("mismatch in length of proof keys (%d)/vals (%d)", len(leafsResponse.ProofKeys), len(leafsResponse.ProofVals))
		}
		proof = memorydb.New()
		defer proof.Close()
		for i, proofKey := range leafsResponse.ProofKeys {
			if err := proof.Put(proofKey, leafsResponse.ProofVals[i]); err != nil {
				return nil, err
			}
		}
	}

	var (
		firstKey = leafsRequest.Start
		lastKey  = leafsRequest.End
	)
	if firstKey == nil {
		firstKey = bytes.Repeat([]byte{0x00}, len(leafsRequest.End))
	}
	// Last key is the last returned key in response
	if len(leafsResponse.Keys) > 0 {
		lastKey = leafsResponse.Keys[len(leafsResponse.Keys)-1]
	}

	// VerifyRangeProof verifies that the key-value pairs included in [leafResponse] are all of the keys within the range from start
	// to the last key returned.
	// Also ensures the keys are in monotonically increasing order
	more, err := trie.VerifyRangeProof(leafsRequest.Root, firstKey, lastKey, leafsResponse.Keys, leafsResponse.Vals, proof)
	if err != nil {
		return nil, fmt.Errorf("%s due to %w", errInvalidRangeProof, err)
	}

	// Set the [More] flag to indicate if there are more leaves to the right of the last key in the response
	// that needs to be fetched.
	leafsResponse.More = more

	return leafsResponse, nil
}

func (c *client) GetBlocks(hash common.Hash, height uint64, parents uint16) ([]*types.Block, error) {
	req := message.BlockRequest{
		Hash:    hash,
		Height:  height,
		Parents: parents,
	}

	data, err := c.get(req, c.maxAttempts, c.maxRetryDelay, c.parseBlocks)
	if err != nil {
		return nil, fmt.Errorf("could not get blocks (%s) due to %w", hash, err)
	}

	return data.(types.Blocks), err
}

// parseBlocks validates given object as message.BlockResponse
// assumes req is of type message.BlockRequest
// returns types.Blocks as interface{}
// returns a non-nil error if the request should be retried
func (c *client) parseBlocks(req message.Request, data []byte) (interface{}, error) {
	var response message.BlockResponse
	if _, err := c.codec.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("%s: %w", errUnmarshalResponse, err)
	}
	if len(response.Blocks) == 0 {
		return nil, errEmptyResponse
	}
	blockRequest := req.(message.BlockRequest)
	numParentsRequested := blockRequest.Parents
	if len(response.Blocks) > int(numParentsRequested) {
		return nil, errTooManyBlocks
	}

	hash := blockRequest.Hash

	// attempt to decode blocks
	blocks := make(types.Blocks, len(response.Blocks))
	for i, blkBytes := range response.Blocks {
		block := new(types.Block)
		if err := rlp.DecodeBytes(blkBytes, block); err != nil {
			return nil, fmt.Errorf("%s: %w", errUnmarshalResponse, err)
		}

		if block.Hash() != hash {
			return nil, fmt.Errorf("%w for block: (got %v) (expected %v)", errHashMismatch, block.Hash(), hash)
		}

		blocks[i] = block
		hash = block.ParentHash()
	}

	// return decoded blocks
	return blocks, nil
}

func (c *client) GetCode(hash common.Hash) ([]byte, error) {
	req := message.NewCodeRequest(hash)

	data, err := c.get(req, c.maxAttempts, c.maxRetryDelay, c.parseCode)
	if err != nil {
		return nil, fmt.Errorf("could not get code (%s): %w", hash, err)
	}

	response := data.(message.CodeResponse)
	return response.Data, err
}

// parseCode validates given object as a code object
// assumes req is of type message.CodeRequest
// returns a non-nil error if the request should be retried
func (c *client) parseCode(req message.Request, data []byte) (interface{}, error) {
	var response message.CodeResponse
	if _, err := c.codec.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	if len(response.Data) == 0 {
		return nil, errEmptyResponse
	}

	hash := crypto.Keccak256Hash(response.Data)
	expected := req.(message.CodeRequest).Hash
	if hash != expected {
		return nil, fmt.Errorf("%w for code: (got %v) (expected %v)", errHashMismatch, hash, expected)
	}

	return response, nil
}

// get submits given request and blockingly returns with either a parsed response object or error
// retry is made if there is a network error or if the [parseResponseFn] returns a non-nil error
// returns parsed struct as interface{} returned by parseResponseFn
// retries given request for maximum of [attempts] times with maximum delay of [maxRetryDelay] between attempts
// Thread safe
func (c *client) get(request message.Request, attempts uint8, maxRetryDelay time.Duration, parseFn parseResponseFn) (interface{}, error) {
	// marshal the request into requestBytes
	requestBytes, err := message.RequestToBytes(c.codec, request)
	if err != nil {
		return nil, err
	}

	var responseIntf interface{}
	// Loop until we run out of attempts or receive a valid response.
	for attempt := uint8(0); attempt < attempts; attempt++ {
		// If this is a retry attempt, wait for random duration to ensure
		// that we do not spin through the maximum attempts during a period
		// where the node may not be well connected to the network.
		if attempt > 0 {
			randTime := rand.Int63n(maxRetryDelay.Nanoseconds())
			time.Sleep(time.Duration(randTime))
		}

		var (
			response []byte
			nodeID   ids.ShortID
		)
		if len(c.stateSyncNodes) == 0 {
			response, nodeID, err = c.networkClient.RequestAny(StateSyncVersion, requestBytes)
		} else {
			// get the next nodeID using the nodeIdx offset. If we're out of nodes, loop back to 0
			// we do this every attempt to ensure we get a different node each time if possible.
			c.nodeIdx = (c.nodeIdx + 1) % len(c.stateSyncNodes)
			nodeID = c.stateSyncNodes[c.nodeIdx]

			response, err = c.networkClient.Request(nodeID, requestBytes)
		}

		if err != nil {
			log.Info("request failed, retrying", "nodeID", nodeID, "attempt", attempt, "request", request, "err", err)
			continue
		} else {
			responseIntf, err = parseFn(request, response)
			if err != nil {
				log.Info("could not validate response, retrying", "nodeID", nodeID, "attempt", attempt, "request", request, "err", err)
				continue
			}
			return responseIntf, nil
		}
	}

	// we only get this far if we've run out of attempts
	return nil, fmt.Errorf("%s (%d): %w", errExceededRetryLimit, attempts, err)
}