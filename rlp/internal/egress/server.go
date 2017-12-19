package egress

import (
	"fmt"
	"io"
	"log"
	"time"

	"code.cloudfoundry.org/loggregator/metricemitter"
	"code.cloudfoundry.org/loggregator/plumbing/batching"
	v2 "code.cloudfoundry.org/loggregator/plumbing/v2"

	"golang.org/x/net/context"
)

const (
	envelopeBufferSize = 10000
)

// HealthRegistrar provides an interface to record various counters.
type HealthRegistrar interface {
	Inc(name string)
	Dec(name string)
}

// Receiver creates a function which will receive envelopes on a stream.
type Receiver interface {
	Subscribe(ctx context.Context, req *v2.EgressBatchRequest) (rx func() (*v2.Envelope, error), err error)
}

// MetricClient creates new CounterMetrics to be emitted periodically.
type MetricClient interface {
	NewCounter(name string, opts ...metricemitter.MetricOption) *metricemitter.Counter
}

// Server represents a bridge between inbound data from the Receiver and
// outbound data on a gRPC stream.
type Server struct {
	receiver      Receiver
	egressMetric  *metricemitter.Counter
	droppedMetric *metricemitter.Counter
	health        HealthRegistrar
	ctx           context.Context
	batchSize     int
	batchInterval time.Duration
}

// NewServer is the preferred way to create a new Server.
func NewServer(
	r Receiver,
	m MetricClient,
	h HealthRegistrar,
	c context.Context,
	batchSize int,
	batchInterval time.Duration,
) *Server {
	egressMetric := m.NewCounter("egress",
		metricemitter.WithVersion(2, 0),
	)

	droppedMetric := m.NewCounter("dropped",
		metricemitter.WithVersion(2, 0),
		metricemitter.WithTags(map[string]string{
			"direction": "egress",
		}),
	)

	return &Server{
		receiver:      r,
		egressMetric:  egressMetric,
		droppedMetric: droppedMetric,
		health:        h,
		ctx:           c,
		batchSize:     batchSize,
		batchInterval: batchInterval,
	}
}

// Receiver implements the loggregator-api V2 gRPC interface for receiving
// envelopes from upstream connections.
func (s *Server) Receiver(r *v2.EgressRequest, srv v2.Egress_ReceiverServer) error {
	s.health.Inc("subscriptionCount")
	defer s.health.Dec("subscriptionCount")

	ctx, cancel := context.WithCancel(srv.Context())
	defer cancel()

	buffer := make(chan *v2.Envelope, envelopeBufferSize)

	r.Selectors = s.convergeSelectors(r.GetLegacySelector(), r.GetSelectors())
	r.LegacySelector = nil

	go func() {
		select {
		case <-s.ctx.Done():
			cancel()
		case <-ctx.Done():
			cancel()
		}
	}()

	// TODO: Error when given legacy selector and selector
	br := &v2.EgressBatchRequest{
		ShardId:          r.GetShardId(),
		LegacySelector:   r.GetLegacySelector(),
		Selectors:        r.GetSelectors(),
		UsePreferredTags: r.GetUsePreferredTags(),
	}

	rx, err := s.receiver.Subscribe(ctx, br)
	if err != nil {
		log.Printf("Unable to setup subscription: %s", err)
		return fmt.Errorf("unable to setup subscription")
	}

	go s.consumeReceiver(buffer, rx, cancel)

	for data := range buffer {
		if err := srv.Send(data); err != nil {
			log.Printf("Send error: %s", err)
			return io.ErrUnexpectedEOF
		}

		// metric-documentation-v2: (loggregator.rlp.egress) Number of v2
		// envelopes sent to RLP consumers.
		s.egressMetric.Increment(1)
	}

	return nil
}

// BatchedReceiver implements the loggregator-api V2 gRPC interface for
// receiving batches of envelopes. Envelopes will be written to the egress
// batched receiver server whenever the configured interval or configured
// batch size is exceeded.
func (s *Server) BatchedReceiver(r *v2.EgressBatchRequest, srv v2.Egress_BatchedReceiverServer) error {
	s.health.Inc("subscriptionCount")
	defer s.health.Dec("subscriptionCount")

	r.Selectors = s.convergeSelectors(r.GetLegacySelector(), r.GetSelectors())
	r.LegacySelector = nil

	ctx, cancel := context.WithCancel(srv.Context())
	defer cancel()

	buffer := make(chan *v2.Envelope, envelopeBufferSize)

	go func() {
		select {
		case <-s.ctx.Done():
			cancel()
		case <-ctx.Done():
			cancel()
		}
	}()

	// TODO: Error if given legacy selector and selector
	rx, err := s.receiver.Subscribe(ctx, r)
	// TODO Add coverage for this error case
	if err != nil {
		log.Printf("Unable to setup subscription: %s", err)
		return fmt.Errorf("unable to setup subscription")
	}

	receiveErrorStream := make(chan error, 1)
	go s.consumeBatchReceiver(buffer, receiveErrorStream, rx, cancel)

	senderErrorStream := make(chan error, 1)
	batcher := batching.NewV2EnvelopeBatcher(
		s.batchSize,
		s.batchInterval,
		&batchWriter{
			srv:          srv,
			errStream:    senderErrorStream,
			egressMetric: s.egressMetric,
		},
	)

	for {
		select {
		case data := <-buffer:
			batcher.Write(data)
		case <-senderErrorStream:
			return io.ErrUnexpectedEOF
		case <-receiveErrorStream:
			for len(buffer) > 0 {
				data := <-buffer
				batcher.Write(data)
			}
			batcher.ForcedFlush()

			return nil
		default:
			batcher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}

	return nil
}

// convergeSelectors takes in any LegacySelector on the request as well as
// Selectors and converts LegacySelector into a Selector based on Selector
// heirarchy.
func (s *Server) convergeSelectors(legacy *v2.Selector, selectors []*v2.Selector) []*v2.Selector {
	if legacy != nil && len(selectors) > 0 {
		// Both would be set by the consumer for upgrade path purposes.
		// The contract should be to assume that the Selectors encompasses
		// the LegacySelector. Therefore, just ignore the LegacySelector.
		return selectors
	}

	if legacy != nil {
		return []*v2.Selector{legacy}
	}

	return selectors
}

type batchWriter struct {
	srv          v2.Egress_BatchedReceiverServer
	errStream    chan<- error
	egressMetric *metricemitter.Counter
}

func (b *batchWriter) Write(batch []*v2.Envelope) {
	err := b.srv.Send(&v2.EnvelopeBatch{Batch: batch})
	if err != nil {
		select {
		case b.errStream <- err:
		default:
		}
		return
	}

	// metric-documentation-v2: (loggregator.rlp.egress) Number of v2
	// envelopes sent to RLP consumers.
	b.egressMetric.Increment(uint64(len(batch)))
}

func (s *Server) consumeBatchReceiver(
	buffer chan<- *v2.Envelope,
	errorStream chan<- error,
	rx func() (*v2.Envelope, error),
	cancel func(),
) {

	defer cancel()

	for {
		e, err := rx()
		if err == io.EOF {
			errorStream <- err
			break
		}

		if err != nil {
			log.Printf("Subscribe error: %s", err)
			errorStream <- err
			break
		}

		select {
		case buffer <- e:
		default:
			// metric-documentation-v2: (loggregator.rlp.dropped) Number of v2
			// envelopes dropped while egressing to a consumer.
			s.droppedMetric.Increment(1)
		}
	}
}

func (s *Server) consumeReceiver(
	buffer chan<- *v2.Envelope,
	rx func() (*v2.Envelope, error),
	cancel func(),
) {

	defer cancel()
	defer close(buffer)

	for {
		e, err := rx()
		if err == io.EOF {
			break
		}

		if err != nil {
			log.Printf("Subscribe error: %s", err)
			break
		}

		select {
		case buffer <- e:
		default:
			// metric-documentation-v2: (loggregator.rlp.dropped) Number of v2
			// envelopes dropped while egressing to a consumer.
			s.droppedMetric.Increment(1)
		}
	}
}
