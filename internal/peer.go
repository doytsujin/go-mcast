package internal

import (
	"context"
	"encoding/json"
	"sync"
)

// Interface that a single peer must implement.
type PartitionPeer interface {
	// The peer transport. Used to send requests
	// to the protocol.
	Transport() Transport

	// A fast read directly into the storage.
	// Since all peers will be consistent, the read
	// operations can be done directly into the storage.
	//
	// See that if a write was issued, is not guaranteed
	// that the read will be executed after the write.
	FastRead(request Request) (Response, error)

	// Stop the peer.
	Stop()
}

// This structure defines a single peer for the protocol.
// A group of peers will form a single partition, so,
// a single peer is not fault tolerant, but a partition
// will be.
type Peer struct {
	// Mutex for synchronizing operations.
	mutex *sync.Mutex

	// Configuration for the peer.
	configuration *PeerConfiguration

	// Transport used for communication between peers
	// and between partitions.
	transport Transport

	// The peer clock for defining a message timestamp.
	clock LogicalClock

	// The peer received queue, to order the requests.
	rqueue Queue

	// Previous set for the peer.
	previousSet PreviousSet

	// Process responsible to deliver messages on the
	// right order.
	deliver Deliverable

	// Holds the peer storage, this will be used
	// for reads only, all writes will come from the
	// state machine when a commit is applied.
	storage Storage

	// Conflict relationship for ordering the messages.
	conflict ConflictRelationship

	// Peer logger.
	log Logger

	// When external requests exchange timestamp,
	// this will hold the received values.
	received *Memo

	// The peer cancellable context.
	context context.Context

	// A cancel function to finish the peer processing.
	finish context.CancelFunc
}

// Creates a new peer for the given configuration and
// start polling for new messages.
func NewPeer(configuration *PeerConfiguration, log Logger) (PartitionPeer, error) {
	t, err := NewTransport(configuration, log)
	if err != nil {
		return nil, err
	}

	ctx, done := context.WithCancel(context.Background())
	p := &Peer{
		mutex:         &sync.Mutex{},
		configuration: configuration,
		transport:     t,
		clock:         &ProcessClock{},
		rqueue:        NewQueue(),
		previousSet:   NewPreviousSet(),
		deliver:       NewDeliver(log, configuration.Conflict, configuration.Storage),
		storage:       configuration.Storage,
		conflict:      configuration.Conflict,
		log:           log,
		received:      NewMemo(),
		context:       ctx,
		finish:        done,
	}
	p.configuration.Invoker.invoke(p.poll)
	return p, nil
}

// Implements the PartitionPeer interface.
func (p *Peer) Transport() Transport {
	return p.transport
}

// Implements the PartitionPeer interface.
func (p *Peer) FastRead(request Request) (Response, error) {
	res := Response{
		Success:    false,
		Identifier: "",
		Data:       nil,
		Extra:      nil,
		Failure:    nil,
	}
	data, err := p.storage.Get(request.Key)
	if err != nil {
		res.Failure = err
		return res, err
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		res.Failure = err
		return res, nil
	}

	res.Success = true
	res.Identifier = entry.Identifier
	res.Data = entry.Data
	res.Extra = entry.Extensions
	return res, nil
}

// Implements the PartitionPeer interface.
func (p *Peer) Stop() {
	defer p.transport.Close()
	p.finish()
}

// This method will keep polling as long as the peer
// is active.
// Listening for messages received from the transport
// and processing following the protocol definition.
// If the context is cancelled, this method will stop.
func (p *Peer) poll() {
	defer p.log.Debugf("closing the peer %s", p.configuration.Name)
	for {
		select {
		case <-p.context.Done():
			return
		case m := <-p.transport.Listen():
			p.log.Debugf("received message %#v", m)
			p.configuration.Invoker.invoke(func() {
				p.process(m)
			})
		case commit := <-p.deliver.Listen():
			p.log.Debugf("message committed. %v", commit)
			p.rqueue.Dequeue(Message{Identifier: commit.Identifier})
			p.received.Remove(commit.Identifier)
		}
	}
}

// Process the received message from the transport.
// First verify if the current configured peer can handle
// this request version.
//
// If the process can be handled, the message is then processed,
// the message must be of type initial or of type external.
//
// After processing the message, updates the value on the
// received queue and then trigger the deliver method to
// start commit on the state machine.
func (p Peer) process(message Message) {
	header := message.Extract()
	if header.ProtocolVersion != p.configuration.Version {
		p.log.Warnf("peer not processing message %v on version %d", message, header.ProtocolVersion)
		return
	}

	valid := false
	defer func() {
		if valid {
			p.rqueue.Enqueue(message)
		}
		p.doDeliver()
	}()

	p.log.Debugf("processing request %v", message)
	switch header.Type {
	case Initial:
		valid = true
		p.processInitialMessage(&message)
	case External:
		valid = true
		p.exchangeTimestamp(&message)
	default:
		p.log.Warnf("unknown message type %d", header.Type)
	}
}

// After the process GB-Deliver m, if m.State is equals to S0, firstly the
// algorithm check if m conflict with any other message on previousSet,
// if so, the process p increment its local clock and empty the previousSet.
// At last, the message m receives its group timestamp and is added to
// previousSet maintaining the information about conflict relations to
// future messages.
//
// On the second part of this procedure, the process p checks if m.Destination
// has only one destination, if so, message m can jump to state S3, since its
// not necessary to exchange timestamp information between others destination
// groups and a second consensus can be avoided due to group timestamp is now a
// final timestamp.
//
// Otherwise, for messages on state S0, we set the group timestamp to the value
// of the process clock, update m.State to S1 and send m to all others groups in
// m.Destination. On the other hand, to messages on state S2, the message has the
// final timestamp, thus m.State can be updated to the final state S3 and, if
// m.Timestamp is greater than local clock value, the clock is updated to hold
// the received timestamp and the previousSet can be cleaned.
func (p *Peer) processInitialMessage(message *Message) {
	if message.State == S0 {
		if p.conflict.Conflict(*message, p.previousSet.Snapshot()) {
			p.clock.Tick()
			p.previousSet.Clear()
		}
		message.Timestamp = p.clock.Tock()
		p.previousSet.Append(*message)
	}

	if len(message.Destination) > 1 {
		if message.State == S0 {
			message.State = S1
			message.Timestamp = p.clock.Tock()
			p.send(*message, External)
		} else if message.State == S2 {
			message.State = S3
			if message.Timestamp > p.clock.Tock() {
				p.clock.Leap(message.Timestamp)
				p.previousSet.Clear()
			}
		}
	} else {
		message.Timestamp = p.clock.Tock()
		message.State = S3
	}
}

// When a message m has more than one destination group, the destination groups
// have to exchange its timestamps to decide the final timestamp to m.
// Thus, after receiving all other timestamp values, a temporary variable tsm is
// agree upon the maximum timestamp value received.
//
// Once the algorithm have select the ts m value, the process checks if m.Timestamp
// is greater or equal to tsm, in positive case, a second consensus instance can be
// avoided and, the state of m can jump directly to state S3 since the group local
// clock is already bigger than tsm.
func (p *Peer) exchangeTimestamp(message *Message) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.received.Insert(message.Identifier, message.Timestamp)
	values := p.received.Read(message.Identifier)
	if len(values) < message.Partitions {
		return
	}

	tsm := MaxValue(values)
	if message.Timestamp >= tsm {
		message.State = S3
	} else {
		message.Timestamp = tsm
		message.State = S2
	}
}

// Used to send a request using the transport API.
// Used for request across partitions, when exchanging the
// message timestamp.
func (p Peer) send(message Message, t MessageType) {
	message.Header.Type = t
	var otherPartitions []Partition
	for _, partition := range message.Destination {
		if partition != p.configuration.Partition {
			otherPartitions = append(otherPartitions, partition)
		}
	}
	message.Destination = otherPartitions
	if err := p.transport.Broadcast(message); err != nil {
		p.log.Errorf("failed exchanging request %v. %v", message, err)
	}
}

// Start the deliver process of the ready messages.
// This will create a snapshot of the messages present
// on the received queue and start delivering messages
// based on the snapshot value.
func (p *Peer) doDeliver() {
	var messages []Message
	for _, m := range p.rqueue.Snapshot() {
		messages = append(messages, m.(Message))
	}

	p.configuration.Invoker.invoke(func() {
		p.deliver.Deliver(messages)
	})
}
