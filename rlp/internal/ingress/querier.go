package ingress

import (
	"log"

	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"golang.org/x/net/context"
)

type ContainerMetricFetcher interface {
	ContainerMetrics(ctx context.Context, appID string) [][]byte
}

type EnvelopeConverter interface {
	Convert(data []byte, usePreferredTags bool) (*loggregator_v2.Envelope, error)
}

type Querier struct {
	fetcher   ContainerMetricFetcher
	converter EnvelopeConverter
}

func NewQuerier(c EnvelopeConverter, f ContainerMetricFetcher) *Querier {
	return &Querier{
		fetcher:   f,
		converter: c,
	}
}

func (q *Querier) ContainerMetrics(ctx context.Context, sourceID string, usePreferredTags bool) ([]*loggregator_v2.Envelope, error) {
	results := q.fetcher.ContainerMetrics(ctx, sourceID)

	var v2Envs []*loggregator_v2.Envelope
	for _, envBytes := range results {
		v2e, err := q.converter.Convert(envBytes, usePreferredTags)
		if err != nil {
			log.Printf("Invalid container envelope: %s", err)
			continue
		}

		v2Envs = append(v2Envs, v2e)
	}

	return v2Envs, nil
}
