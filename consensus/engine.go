// Copyright (c) 2022 Project Illium
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.

package consensus

import (
	"context"
	"fmt"
	ctxio "github.com/jbenet/go-context/io"
	inet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"github.com/project-illium/ilxd/blockchain"
	"github.com/project-illium/ilxd/net"
	"github.com/project-illium/ilxd/params"
	"github.com/project-illium/ilxd/types"
	"github.com/project-illium/ilxd/types/wire"
	"google.golang.org/protobuf/proto"
	"io"
	"math/rand"
	"sync"
	"time"
)

func init() {
	rand.Seed(time.Now().Unix())
}

const (
	// AvalancheRequestTimeout is the amount of time to wait for a response to a
	// query
	AvalancheRequestTimeout = 1 * time.Minute

	// AvalancheFinalizationScore is the confidence score we consider to be final
	AvalancheFinalizationScore = 128

	// AvalancheTimeStep is the amount of time to wait between event ticks
	AvalancheTimeStep = time.Millisecond

	// AvalancheMaxInflightPoll is the max outstanding requests that we can have
	// for any inventory item.
	AvalancheMaxInflightPoll = 10

	// AvalancheMaxElementPoll is the maximum number of invs to send in a single
	// query
	AvalancheMaxElementPoll = 4096

	// DeleteInventoryAfter is the maximum time we'll keep a block in memory
	// if it hasn't been finalized by avalanche.
	DeleteInventoryAfter = time.Hour * 6

	ConsensusProtocol = "consensus"
)

// requestExpirationMsg signifies a request has expired and
// should be removed from the map.
type requestExpirationMsg struct {
	key string
}

// queryMsg signifies a query from another peer.
type queryMsg struct {
	request  *wire.MsgAvaRequest
	respChan chan *wire.MsgAvaResponse
}

type newBlockMessage struct {
	blockID                     types.ID
	initialAcceptancePreference bool
	callback                    chan<- Status
}

// registerVotesMsg signifies a response to a query from another peer.
type registerVotesMsg struct {
	p    peer.ID
	resp *wire.MsgAvaResponse
}

type ConsensusEngine struct {
	ctx     context.Context
	network *net.Network
	params  *params.NetworkParams
	chooser blockchain.WeightedChooser
	ms      net.MessageSender
	wg      sync.WaitGroup
	quit    chan struct{}
	msgChan chan interface{}

	voteRecords    map[types.ID]*VoteRecord
	rejectedBlocks map[types.ID]struct{}
	queries        map[string]RequestRecord
	callbacks      map[types.ID]chan<- Status
	streams        map[peer.ID]inet.Stream
	start          time.Time

	alwaysNo    bool
	flipFlopper bool
	flipperVote bool
	printState  bool
}

func NewConsensusEngine(ctx context.Context, params *params.NetworkParams, network *net.Network, chooser blockchain.WeightedChooser) (*ConsensusEngine, error) {
	return &ConsensusEngine{
		ctx:            ctx,
		network:        network,
		chooser:        chooser,
		params:         params,
		ms:             net.NewMessageSender(network.Host(), params.ProtocolPrefix+ConsensusProtocol),
		wg:             sync.WaitGroup{},
		quit:           make(chan struct{}),
		msgChan:        make(chan interface{}),
		voteRecords:    make(map[types.ID]*VoteRecord),
		rejectedBlocks: make(map[types.ID]struct{}),
		queries:        make(map[string]RequestRecord),
		callbacks:      make(map[types.ID]chan<- Status),
	}, nil
}

// Start begins the core handler which processes peers and avalanche messages.
func (eng *ConsensusEngine) Start() {
	eng.network.Host().SetStreamHandler(eng.params.ProtocolPrefix+ConsensusProtocol, eng.HandleNewStream)
	eng.wg.Add(1)
	go eng.handler()
}

func (eng *ConsensusEngine) Stop() {
	close(eng.quit)
	eng.wg.Wait()
}

func (eng *ConsensusEngine) handler() {
	eventLoopTicker := time.NewTicker(AvalancheTimeStep)
out:
	for {
		select {
		case m := <-eng.msgChan:
			switch msg := m.(type) {
			case *requestExpirationMsg:
				eng.handleRequestExpiration(msg.key)
			case *queryMsg:
				eng.handleQuery(msg.request, msg.respChan)
			case *newBlockMessage:
				eng.handleNewBlock(msg.blockID, msg.initialAcceptancePreference, msg.callback)
			case *registerVotesMsg:
				eng.handleRegisterVotes(msg.p, msg.resp)
			}
		case <-eventLoopTicker.C:
			eng.pollLoop()
		case <-eng.quit:
			break out
		}
	}
	eventLoopTicker.Stop()
	eng.wg.Done()
}

func (eng *ConsensusEngine) NewBlock(blockID types.ID, initialAcceptancePreference bool, callback chan<- Status) {
	eng.start = time.Now()
	eng.msgChan <- &newBlockMessage{
		blockID:                     blockID,
		initialAcceptancePreference: initialAcceptancePreference,
		callback:                    callback,
	}
}

func (eng *ConsensusEngine) handleNewBlock(blockID types.ID, initialAcceptancePreference bool, callback chan<- Status) {
	_, ok := eng.voteRecords[blockID]
	if ok {
		return
	}
	_, ok = eng.rejectedBlocks[blockID]
	if ok {
		return
	}

	vr := NewVoteRecord(blockID, initialAcceptancePreference)
	eng.voteRecords[blockID] = vr

	eng.callbacks[blockID] = callback
}

func (eng *ConsensusEngine) HandleNewStream(s inet.Stream) {
	go eng.handleNewMessage(s)
}

func (eng *ConsensusEngine) handleNewMessage(s inet.Stream) {
	defer s.Close()
	contextReader := ctxio.NewReader(eng.ctx, s)
	reader := msgio.NewVarintReaderSize(contextReader, inet.MessageSizeMax)
	remotePeer := s.Conn().RemotePeer()
	defer reader.Close()

	for {
		select {
		case <-eng.ctx.Done():
			return
		default:
		}

		req := new(wire.MsgAvaRequest)
		msgBytes, err := reader.ReadMsg()
		if err != nil {
			reader.ReleaseMsg(msgBytes)
			s.Reset()
			if err == io.EOF {
				log.Debugf("Peer %s closed avalanche stream", remotePeer)
			}
			return
		}
		if err := proto.Unmarshal(msgBytes, req); err != nil {
			reader.ReleaseMsg(msgBytes)
			continue
		}
		reader.ReleaseMsg(msgBytes)

		respCh := make(chan *wire.MsgAvaResponse)
		eng.msgChan <- &queryMsg{
			request:  req,
			respChan: respCh,
		}

		respMsg := <-respCh
		err = net.WriteMsg(s, respMsg)
		if err != nil {
			log.Errorf("Error writing avalanche stream to peer %d", remotePeer)
			s.Reset()
		}
	}
}

func (eng *ConsensusEngine) handleQuery(req *wire.MsgAvaRequest, respChan chan *wire.MsgAvaResponse) {
	votes := make([]byte, len(req.Invs))
	for i, invBytes := range req.Invs {
		inv := types.NewID(invBytes)

		if _, exists := eng.rejectedBlocks[inv]; exists {
			votes[i] = 0x00 // No vote
			continue
		}
		record, ok := eng.voteRecords[inv]
		if ok {
			// We're only going to vote for items we have a record for.
			votes[i] = 0x00 // No vote
			if record.isPreferred() {
				votes[i] = 0x01 // Yes vote
			}
		} else {
			// TODO: we need to download this block from the peer and give it to
			// the mempool for processing.

			votes[i] = 0x80 // Neutral vote
		}

		if eng.alwaysNo {
			votes[i] = 0x00
			continue
		}

		if eng.flipFlopper {
			votes[i] = boolToUint8(eng.flipperVote)
			eng.flipperVote = !eng.flipperVote
		}
	}
	resp := &wire.MsgAvaResponse{
		RequestID: req.RequestID,
		Votes:     votes,
	}

	respChan <- resp
}

func (eng *ConsensusEngine) handleRequestExpiration(key string) {
	r, ok := eng.queries[key]
	if !ok {
		return
	}
	delete(eng.queries, key)

	invs := r.GetInvs()
	for inv := range invs {
		vr, ok := eng.voteRecords[inv]
		if ok {
			vr.inflightRequests--
		}
	}
}

func (eng *ConsensusEngine) queueMessageToPeer(req *wire.MsgAvaRequest, peer peer.ID) {
	var (
		key  = queryKey(req.RequestID, peer.String())
		resp = new(wire.MsgAvaResponse)
	)

	err := eng.ms.SendRequest(eng.ctx, peer, req, resp)
	if err != nil {
		log.Errorf("Error reading avalanche response from peer %s", peer.String())
		eng.msgChan <- &requestExpirationMsg{key}
		return
	}

	eng.msgChan <- &registerVotesMsg{
		p:    peer,
		resp: resp,
	}
}

func (eng *ConsensusEngine) handleRegisterVotes(p peer.ID, resp *wire.MsgAvaResponse) {
	key := queryKey(resp.RequestID, p.String())

	r, ok := eng.queries[key]
	if !ok {
		log.Debugf("Received avalanche response from peer %s with an unknown request ID", p)
		return
	}

	// Always delete the key if it's present
	delete(eng.queries, key)

	if r.IsExpired() {
		log.Debugf("Received avalanche response from peer %s with an expired request", p)
		return
	}

	invs := r.GetInvs()
	if len(resp.Votes) != len(invs) {
		log.Debugf("Received avalanche response from peer %s with incorrect number of votes", p)
		return
	}

	i := -1
	for inv := range invs {
		i++
		vr, ok := eng.voteRecords[inv]
		if !ok {
			// We are not voting on this anymore
			continue
		}
		vr.inflightRequests--

		if vr.hasFinalized() {
			continue
		}

		if !vr.regsiterVote(resp.Votes[i]) {
			if eng.printState {
				vr.printState()
			}
			// This vote did not provide any extra information
			continue
		}

		if vr.isPreferred() {
			// We need to keep track of conflicting blocks
			// when this one becomes accepted we need to set the
			// confidence of the conflicts back to zero.
		}

		if vr.status() == StatusFinalized || vr.status() == StatusRejected {
			if eng.printState {
				vr.printState()
			}
			callback, ok := eng.callbacks[inv]
			if ok {
				go func() {
					callback <- vr.status()
				}()
			}
		}
	}
}

func (eng *ConsensusEngine) pollLoop() {
	if eng.alwaysNo || eng.flipFlopper {
		return
	}
	invs := eng.getInvsForNextPoll()
	if len(invs) == 0 {
		return
	}

	p := eng.chooser.WeightedRandomValidator()
	if p == "" {
		return
	}
	requestID := rand.Uint32()

	key := queryKey(requestID, p.String())
	eng.queries[key] = NewRequestRecord(time.Now().Unix(), invs)

	invList := make([][]byte, 0, len(invs))
	for _, inv := range invs {
		invList = append(invList, inv[:])
	}

	req := &wire.MsgAvaRequest{
		RequestID: requestID,
		Invs:      invList,
	}

	go eng.queueMessageToPeer(req, p)
}

func (eng *ConsensusEngine) getInvsForNextPoll() []types.ID {
	var (
		invs     []types.ID
		toDelete []types.ID
	)
	for id, r := range eng.voteRecords {
		// Delete very old inventory that hasn't finalized
		if time.Since(r.timestamp) > DeleteInventoryAfter {
			toDelete = append(toDelete, id)
			continue
		}

		if r.hasFinalized() {
			// If this has finalized we can just skip.
			continue
		}
		if r.inflightRequests >= AvalancheMaxInflightPoll {
			// If we are already at the max inflight then continue
			continue
		}
		r.inflightRequests++

		// We don't have a decision, we need more votes.
		invs = append(invs, id)
	}

	if len(invs) >= AvalancheMaxElementPoll {
		invs = invs[:AvalancheMaxElementPoll]
	}

	for _, td := range toDelete {
		delete(eng.voteRecords, td)
	}

	return invs
}

func queryKey(requestID uint32, peerID string) string {
	return fmt.Sprintf("%d|%s", requestID, peerID)
}
