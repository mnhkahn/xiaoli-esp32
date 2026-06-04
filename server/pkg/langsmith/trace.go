package langsmith

import (
	"context"
	"sync"

	"golang.org/x/exp/slices"
)

type langsmithTraceOptionKey struct{}

type traceOptions struct {
	SessionName        string
	ReferenceExampleID string
	TraceID            string
	Metadata           *sync.Map
	ParentID           string
	ParentDottedOrder  string
	Tags               []string
}

type TraceOption func(*traceOptions)

func SetTrace(ctx context.Context, opts ...TraceOption) context.Context {
	options := &traceOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return context.WithValue(ctx, langsmithTraceOptionKey{}, options)
}

func WithSessionName(name string) TraceOption {
	return func(o *traceOptions) {
		o.SessionName = name
	}
}

func AddTag(tag string) TraceOption {
	return func(o *traceOptions) {
		if o.Tags == nil {
			o.Tags = []string{}
		}
		if !slices.Contains(o.Tags, tag) {
			o.Tags = append(o.Tags, tag)
		}
	}
}

func WithReferenceExampleID(id string) TraceOption {
	return func(o *traceOptions) {
		o.ReferenceExampleID = id
	}
}

func WithTraceID(id string) TraceOption {
	return func(o *traceOptions) {
		o.TraceID = id
	}
}

func SetMetadata(metadata *sync.Map) TraceOption {
	return func(o *traceOptions) {
		if o.Metadata == nil {
			o.Metadata = metadata
		} else {
			metadata.Range(func(k, v interface{}) bool {
				o.Metadata.Store(k, v)
				return true
			})
		}
	}
}
