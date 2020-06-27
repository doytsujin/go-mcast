package internal

import (
	"context"
	"encoding/json"
	"github.com/jabolina/relt/pkg/relt"
	"github.com/prometheus/common/log"
)

// The transport interface providing the communication
// primitives by the protocol.
type Transport interface {
	// Reliably deliver the message to all correct processes
	// in the same order.
	// The protocol definition requires this to be a generic
	// broadcast communication primitive, but the one used here
	// will be a total order broadcast.
	Broadcast(message Message) error

	// Listen for messages that arrives on the transport.
	Listen() <-chan Message

	// Close the transport for sending and receiving messages.
	Close()
}

// An instance of the Transport interface that
// provides the required reliable transport primitives.
type ReliableTransport struct {
	// Transport logger.
	log Logger

	// Reliable transport.
	relt *relt.Relt

	// Channel to publish the receiving messages.
	producer chan Message

	// The transport context.
	context context.Context

	// The finish function to closing the transport.
	finish context.CancelFunc
}

// Create a new instance of the transport interface.
func NewTransport(peer *PeerConfiguration, log Logger) (Transport, error) {
	conf := relt.DefaultReltConfiguration()
	conf.Name = peer.Name
	conf.Exchange = relt.GroupAddress(peer.Partition)
	r := relt.NewRelt(*conf)
	ctx, done := context.WithCancel(context.Background())
	t := &ReliableTransport{
		log:      log,
		relt:     r,
		producer: make(chan Message),
		context:  ctx,
		finish:   done,
	}
	peer.Invoker.invoke(t.poll)
	return t, nil
}

// ReliableTransport implements Transport interface.
func (r *ReliableTransport) Broadcast(message Message) error {
	data, err := json.Marshal(message)
	if err != nil {
		log.Errorf("failed marshalling message %#v. %v", message, err)
		return err
	}

	for _, partition := range message.Destination {
		m := relt.Send{
			Address: relt.GroupAddress(partition),
			Data:    data,
		}
		r.log.Debugf("broadcasting message to %s", partition)
		if err = r.relt.Broadcast(m); err != nil {
			r.log.Errorf("failed sending %#v. %v", m, err)
			return err
		}
	}
	return nil
}

// ReliableTransport implements Transport interface.
func (r *ReliableTransport) Listen() <-chan Message {
	return r.producer
}

// ReliableTransport implements Transport interface.
func (r *ReliableTransport) Close() {
	defer close(r.producer)
	r.relt.Close()
	r.finish()
}

// This method will keep polling until
// the transport context cancelled.
// The messages that arrives through the underlying
// transport channel will be sent to the consume
// method to be parsed and publish to the listeners.
func (r ReliableTransport) poll() {
	for {
		select {
		case recv := <-r.relt.Consume():
			r.consume(recv)
		case <-r.context.Done():
			return
		}
	}
}

// Consume will receive a message from the transport
// and will parse into a valid object to be consumed
// by the channel listener.
func (r *ReliableTransport) consume(recv relt.Recv) {
	if recv.Error != nil {
		r.log.Errorf("failed consuming message. %v", recv.Error)
		return
	}

	if recv.Data == nil {
		return
	}

	var m Message
	if err := json.Unmarshal(recv.Data, &m); err != nil {
		r.log.Errorf("failed unmarshalling message %#v. %v", recv, err)
		return
	}
	r.producer <- m
}
