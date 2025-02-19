// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/m3db/m3/src/cmd/services/m3coordinator/ingest"
	"github.com/m3db/m3/src/dbnode/client"
	"github.com/m3db/m3/src/metrics/policy"
	"github.com/m3db/m3/src/query/api/v1/handler/prometheus"
	"github.com/m3db/m3/src/query/api/v1/handler/prometheus/handleroptions"
	"github.com/m3db/m3/src/query/api/v1/options"
	"github.com/m3db/m3/src/query/api/v1/route"
	"github.com/m3db/m3/src/query/generated/proto/prompb"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/storage/m3/storagemetadata"
	"github.com/m3db/m3/src/query/ts"
	"github.com/m3db/m3/src/query/util/logging"
	"github.com/m3db/m3/src/x/clock"
	xerrors "github.com/m3db/m3/src/x/errors"
	"github.com/m3db/m3/src/x/headers"
	"github.com/m3db/m3/src/x/instrument"
	xhttp "github.com/m3db/m3/src/x/net/http"
	"github.com/m3db/m3/src/x/retry"
	xsync "github.com/m3db/m3/src/x/sync"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/golang/protobuf/proto"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// PromWriteURL is the url for the prom write handler
	PromWriteURL = route.Prefix + "/prom/remote/write"

	// PromWriteHTTPMethod is the HTTP method used with this resource.
	PromWriteHTTPMethod = http.MethodPost

	// emptyStoragePolicyVar for code readability.
	emptyStoragePolicyVar = ""

	// defaultForwardingTimeout is the default forwarding timeout.
	defaultForwardingTimeout = 15 * time.Second

	// maxLiteralIsTooLongLogCount is the number of times the time series labels should be logged
	// upon "literal is too long" error.
	maxLiteralIsTooLongLogCount = 10
	// literalPrefixLength is the length of the label literal prefix that is logged upon
	// "literal is too long" error.
	literalPrefixLength = 100
)

var (
	errNoDownsamplerAndWriter       = errors.New("no downsampler and writer set")
	errNoTagOptions                 = errors.New("no tag options set")
	errNoNowFn                      = errors.New("no now fn set")
	errUnaggregatedStoragePolicySet = errors.New("storage policy should not be set for unaggregated metrics")

	defaultForwardingRetryForever = false
	defaultForwardingRetryJitter  = true
	defaultForwardRetryConfig     = retry.Configuration{
		InitialBackoff: time.Second * 2,
		BackoffFactor:  2,
		MaxRetries:     1,
		Forever:        &defaultForwardingRetryForever,
		Jitter:         &defaultForwardingRetryJitter,
	}

	defaultAttribution = make([]*promAttributionMetrics, 0)

	defaultValue = ingest.IterValue{
		Tags:       models.EmptyTags(),
		Attributes: ts.DefaultSeriesAttributes(),
		Metadata:   ts.Metadata{},
	}

	headerToMetricType = map[string]prompb.MetricType{
		"counter":         prompb.MetricType_COUNTER,
		"gauge":           prompb.MetricType_GAUGE,
		"gauge_histogram": prompb.MetricType_GAUGE_HISTOGRAM,
		"histogram":       prompb.MetricType_HISTOGRAM,
		"info":            prompb.MetricType_INFO,
		"stateset":        prompb.MetricType_STATESET,
		"summary":         prompb.MetricType_SUMMARY,
	}
)

type PromLabels struct {
	lbls []prompb.Label
}

func (p PromLabels) MarshalLogArray(encoder zapcore.ArrayEncoder) error {
	for _, label := range p.lbls {
		encoder.AppendString(fmt.Sprintf("%s=%s,", label.Name, label.Value))
	}
	return nil
}

// this option is used to deal with incoming remote write requests
// it is a reflection of RemoteWriteConfiguration
type remoteWriteOpts struct {
	rejectOldSamples  bool
	rejectDuration    time.Duration
	errorSamplingRate float32
}

// PromWriteHandler represents a handler for prometheus write endpoint.
type PromWriteHandler struct {
	downsamplerAndWriter   ingest.DownsamplerAndWriter
	tagOptions             models.TagOptions
	storeMetricsType       bool
	forwarding             handleroptions.PromWriteHandlerForwardingOptions
	forwardTimeout         time.Duration
	forwardHTTPClient      *http.Client
	forwardingBoundWorkers xsync.WorkerPool
	forwardContext         context.Context
	forwardRetrier         retry.Retrier
	nowFn                  clock.NowFn
	instrumentOpts         instrument.Options
	metrics                promWriteMetrics
	attributions           []*promAttributionMetrics
	remoteWriteOpts        remoteWriteOpts

	// Counting the number of times of "literal is too long" error for log sampling purposes.
	numLiteralIsTooLong uint32
}

// NewPromWriteHandler returns a new instance of handler.
func NewPromWriteHandler(options options.HandlerOptions) (http.Handler, error) {
	var (
		downsamplerAndWriter = options.DownsamplerAndWriter()
		tagOptions           = options.TagOptions()
		nowFn                = options.NowFn()
		forwarding           = options.Config().WriteForwarding.PromRemoteWrite
		instrumentOpts       = options.InstrumentOpts()
		logger               = instrumentOpts.Logger()
	)

	if downsamplerAndWriter == nil {
		return nil, errNoDownsamplerAndWriter
	}

	if tagOptions == nil {
		return nil, errNoTagOptions
	}

	if nowFn == nil {
		return nil, errNoNowFn
	}

	scope := options.InstrumentOpts().
		MetricsScope().
		Tagged(map[string]string{"handler": "remote-write"})
	metrics, err := newPromWriteMetrics(scope)
	if err != nil {
		return nil, err
	}

	remoteWriteOpts := remoteWriteOpts{
		rejectOldSamples:  options.Config().RemoteWrite.RejectOldSamples,
		rejectDuration:    options.Config().RemoteWrite.RejectDuration,
		errorSamplingRate: options.Config().RemoteWrite.ErrorSamplingRate,
	}
	logger.Info("set up remote write options",
		zap.Bool("reject old samples", remoteWriteOpts.rejectOldSamples),
		zap.Duration("reject duration", remoteWriteOpts.rejectDuration),
	)

	attributions := defaultAttribution
	if options.Config().Metrics != nil && options.Config().Metrics.Attributions != nil {
		attributions := make([]*promAttributionMetrics, len(options.Config().Metrics.Attributions))
		for i, attributionOpts := range options.Config().Metrics.Attributions {
			attribution, _ := newPromAttributionMetrics(scope, attributionOpts, logger)
			attributions[i] = attribution
		}
	}

	// Only use a forwarding worker pool if concurrency is bound, otherwise
	// if unlimited we just spin up a goroutine for each incoming write.
	var forwardingBoundWorkers xsync.WorkerPool
	if v := forwarding.MaxConcurrency; v > 0 {
		forwardingBoundWorkers = xsync.NewWorkerPool(v)
		forwardingBoundWorkers.Init()
	}

	forwardTimeout := defaultForwardingTimeout
	if v := forwarding.Timeout; v > 0 {
		forwardTimeout = v
	}

	forwardHTTPOpts := xhttp.DefaultHTTPClientOptions()
	forwardHTTPOpts.DisableCompression = true // Already snappy compressed.
	forwardHTTPOpts.RequestTimeout = forwardTimeout

	forwardRetryConfig := defaultForwardRetryConfig
	if forwarding.Retry != nil {
		forwardRetryConfig = *forwarding.Retry
	}
	forwardRetryOpts := forwardRetryConfig.NewOptions(
		scope.SubScope("forwarding-retry"),
	)
	if len(forwarding.Targets) > 0 {
		target := forwarding.Targets[0]
		logger.Info("write forwarding is setup", zap.String("remote-url", target.URL),
			zap.Any("headers", target.Headers), zap.String("tenant-header", target.TenantHeader),
			zap.Duration("timeout", forwardTimeout),
			zap.Any("retries", forwardRetryConfig))
	}
	return &PromWriteHandler{
		downsamplerAndWriter:   downsamplerAndWriter,
		tagOptions:             tagOptions,
		storeMetricsType:       options.StoreMetricsType(),
		forwarding:             forwarding,
		forwardTimeout:         forwardTimeout,
		forwardHTTPClient:      xhttp.NewHTTPClient(forwardHTTPOpts),
		forwardingBoundWorkers: forwardingBoundWorkers,
		forwardContext:         context.Background(),
		forwardRetrier:         retry.NewRetrier(forwardRetryOpts),
		nowFn:                  nowFn,
		metrics:                metrics,
		attributions:           attributions,
		instrumentOpts:         instrumentOpts,
		remoteWriteOpts:        remoteWriteOpts,
	}, nil
}

type promWriteMetrics struct {
	writeSuccess             tally.Counter
	writeRejectTooOld        tally.Counter
	writeErrorsServer        tally.Counter
	writeErrorsClient        tally.Counter
	writeBatchLatency        tally.Histogram
	writeBatchLatencyBuckets tally.DurationBuckets
	ingestLatency            tally.Histogram
	ingestLatencyBuckets     tally.DurationBuckets
	forwardSuccess           tally.Counter
	forwardErrors            tally.Counter
	forwardDropped           tally.Counter
	forwardLatency           tally.Histogram
}

func (m *promWriteMetrics) incError(err error) {
	if xhttp.IsClientError(err) {
		m.writeErrorsClient.Inc(1)
	} else {
		m.writeErrorsServer.Inc(1)
	}
}

func newPromWriteMetrics(scope tally.Scope) (promWriteMetrics, error) {
	buckets, err := ingest.NewLatencyBuckets()
	if err != nil {
		return promWriteMetrics{}, err
	}
	return promWriteMetrics{
		writeSuccess:             scope.SubScope("write").Counter("success"),
		writeRejectTooOld:        scope.SubScope("write").Counter("reject"),
		writeErrorsServer:        scope.SubScope("write").Tagged(map[string]string{"code": "5XX"}).Counter("errors"),
		writeErrorsClient:        scope.SubScope("write").Tagged(map[string]string{"code": "4XX"}).Counter("errors"),
		writeBatchLatency:        scope.SubScope("write").Histogram("batch-latency", buckets.WriteLatencyBuckets),
		writeBatchLatencyBuckets: buckets.WriteLatencyBuckets,
		ingestLatency:            scope.SubScope("ingest").Histogram("latency", buckets.IngestLatencyBuckets),
		ingestLatencyBuckets:     buckets.IngestLatencyBuckets,
		forwardSuccess:           scope.SubScope("forward").Counter("success"),
		forwardErrors:            scope.SubScope("forward").Counter("errors"),
		forwardDropped:           scope.SubScope("forward").Counter("dropped"),
		forwardLatency:           scope.SubScope("forward").Histogram("latency", buckets.WriteLatencyBuckets),
	}, nil
}

func (h *PromWriteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	batchRequestStopwatch := h.metrics.writeBatchLatency.Start()
	defer batchRequestStopwatch.Stop()

	checkedReq, err := h.checkedParseRequest(r)
	if err != nil {
		h.metrics.incError(err)
		xhttp.WriteError(w, err)
		return
	}

	var (
		req    = checkedReq.Request
		opts   = checkedReq.Options
		result = checkedReq.CompressResult
	)
	// Begin async forwarding.
	// NB(r): Be careful about not returning buffers to pool
	// if the request bodies ever get pooled until after
	// forwarding completes.
	if targets := h.forwarding.Targets; len(targets) > 0 {
		for _, target := range targets {
			target := target // Capture for lambda.
			forward := func() {
				now := h.nowFn()
				err := h.forwardRetrier.Attempt(func() error {
					// Consider propagating baggage without tying
					// context to request context in future.
					ctx, cancel := context.WithTimeout(h.forwardContext, h.forwardTimeout)
					defer cancel()
					return h.forward(ctx, result, r.Header, target)
				})

				// Record forward ingestion delay.
				// NB: this includes any time for retries.
				for _, series := range req.Timeseries {
					for _, sample := range series.Samples {
						age := now.Sub(storage.PromTimestampToTime(sample.Timestamp))
						h.metrics.forwardLatency.RecordDuration(age)
					}
				}

				if err != nil {
					h.metrics.forwardErrors.Inc(1)
					logger := logging.WithContext(h.forwardContext, h.instrumentOpts)
					logger.Error("forward error", zap.Error(err))
					return
				}

				h.metrics.forwardSuccess.Inc(1)
			}

			spawned := false
			if h.forwarding.MaxConcurrency > 0 {
				spawned = h.forwardingBoundWorkers.GoIfAvailable(forward)
			} else {
				go forward()
				spawned = true
			}
			if !spawned {
				h.metrics.forwardDropped.Inc(1)
			}
		}
	}

	batchErr := h.write(r.Context(), req, opts)

	// Record ingestion delay latency
	now := h.nowFn()
	for _, series := range req.Timeseries {
		for _, attribution := range h.attributions {
			attribution.attribute(series)
		}
		for _, sample := range series.Samples {
			age := now.Sub(storage.PromTimestampToTime(sample.Timestamp))
			h.metrics.ingestLatency.RecordDuration(age)
		}
	}

	if batchErr != nil {
		var (
			errs                 = batchErr.Errors()
			lastRegularErr       string
			lastBadRequestErr    string
			numRegular           int
			numBadRequest        int
			numResourceExhausted int
		)
		for _, err := range errs {
			switch {
			case client.IsResourceExhaustedError(err):
				numResourceExhausted++
				lastBadRequestErr = err.Error()
			case client.IsBadRequestError(err):
				numBadRequest++
				lastBadRequestErr = err.Error()
			case xerrors.IsInvalidParams(err):
				numBadRequest++
				lastBadRequestErr = err.Error()
			default:
				numRegular++
				lastRegularErr = err.Error()
			}
		}

		var status int
		switch {
		case numBadRequest == len(errs):
			status = http.StatusBadRequest
		case numResourceExhausted > 0:
			status = http.StatusTooManyRequests
		default:
			status = http.StatusInternalServerError
		}

		logger := logging.WithContext(r.Context(), h.instrumentOpts)
		logger.Error("write error",
			zap.String("remoteAddr", r.RemoteAddr),
			zap.Int("httpResponseStatusCode", status),
			zap.Int("numResourceExhaustedErrors", numResourceExhausted),
			zap.Int("numRegularErrors", numRegular),
			zap.Int("numBadRequestErrors", numBadRequest),
			zap.String("lastRegularError", lastRegularErr),
			zap.String("lastBadRequestErr", lastBadRequestErr))

		var resultErrMessage string
		if lastRegularErr != "" {
			resultErrMessage = fmt.Sprintf("retryable_errors: count=%d, last=%s",
				numRegular, lastRegularErr)
		}
		if lastBadRequestErr != "" {
			var sep string
			if lastRegularErr != "" {
				sep = ", "
			}
			resultErrMessage = fmt.Sprintf("%s%sbad_request_errors: count=%d, last=%s",
				resultErrMessage, sep, numBadRequest, lastBadRequestErr)
		}

		resultError := xhttp.NewError(errors.New(resultErrMessage), status)
		h.metrics.incError(resultError)
		xhttp.WriteError(w, resultError)
		return
	}

	// NB(schallert): this is frustrating but if we don't explicitly write an HTTP
	// status code (or via Write()), OpenTracing middleware reports code=0 and
	// shows up as error.
	w.WriteHeader(200)
	h.metrics.writeSuccess.Inc(1)
}

type parseRequestResult struct {
	Request        *prompb.WriteRequest
	Options        ingest.WriteOptions
	CompressResult prometheus.ParsePromCompressedRequestResult
}

func (h *PromWriteHandler) checkedParseRequest(
	r *http.Request,
) (parseRequestResult, error) {
	result, err := h.parseRequest(r)
	if err != nil {
		// Always invalid request if parsing fails params.
		return parseRequestResult{}, xerrors.NewInvalidParamsError(err)
	}
	return result, nil
}

// parseRequest extracts the Prometheus write request from the request body and
// headers. WARNING: it is not guaranteed that the tags returned in the request
// body are in sorted order. It is expected that the caller ensures the tags are
// sorted before passing them to storage, which currently happens in write() ->
// newTSPromIter() -> storage.PromLabelsToM3Tags() -> tags.AddTags(). This is
// the only path written metrics are processed, but future write paths must
// uphold the same guarantees.
func (h *PromWriteHandler) parseRequest(
	r *http.Request,
) (parseRequestResult, error) {
	var opts ingest.WriteOptions
	if v := strings.TrimSpace(r.Header.Get(headers.MetricsTypeHeader)); v != "" {
		// Allow the metrics type and storage policies to override
		// the default rules and policies if specified.
		metricsType, err := storagemetadata.ParseMetricsType(v)
		if err != nil {
			return parseRequestResult{}, err
		}

		// Ensure ingest options specify we are overriding the
		// downsampling rules with zero rules to be applied (so
		// only direct writes will be made).
		opts.DownsampleOverride = true
		opts.DownsampleMappingRules = nil

		strPolicy := strings.TrimSpace(r.Header.Get(headers.MetricsStoragePolicyHeader))
		switch metricsType {
		case storagemetadata.UnaggregatedMetricsType:
			if strPolicy != emptyStoragePolicyVar {
				return parseRequestResult{}, errUnaggregatedStoragePolicySet
			}
		default:
			parsed, err := policy.ParseStoragePolicy(strPolicy)
			if err != nil {
				err = fmt.Errorf("could not parse storage policy: %v", err)
				return parseRequestResult{}, err
			}

			// Make sure this specific storage policy is used for the writes.
			opts.WriteOverride = true
			opts.WriteStoragePolicies = policy.StoragePolicies{
				parsed,
			}
		}
	}
	if v := strings.TrimSpace(r.Header.Get(headers.WriteTypeHeader)); v != "" {
		switch v {
		case headers.DefaultWriteType:
		case headers.AggregateWriteType:
			opts.WriteOverride = true
			opts.WriteStoragePolicies = policy.StoragePolicies{}
		default:
			err := fmt.Errorf("unrecognized write type: %s", v)
			return parseRequestResult{}, err
		}
	}

	result, err := prometheus.ParsePromCompressedRequest(r)
	if err != nil {
		return parseRequestResult{}, err
	}

	var req prompb.WriteRequest
	if err := proto.Unmarshal(result.UncompressedBody, &req); err != nil {
		return parseRequestResult{}, err
	}

	if mapStr := r.Header.Get(headers.MapTagsByJSONHeader); mapStr != "" {
		var opts handleroptions.MapTagsOptions
		if err := json.Unmarshal([]byte(mapStr), &opts); err != nil {
			return parseRequestResult{}, err
		}

		if err := mapTags(&req, opts); err != nil {
			return parseRequestResult{}, err
		}
	}

	if promType := r.Header.Get(headers.PromTypeHeader); promType != "" {
		tp, ok := headerToMetricType[strings.ToLower(promType)]
		if !ok {
			return parseRequestResult{}, fmt.Errorf("unknown prom metric type %s", promType)
		}
		for i := range req.Timeseries {
			req.Timeseries[i].Type = tp
		}
	}

	if h.remoteWriteOpts.rejectOldSamples {
		now := h.nowFn()
		thresholdTime := now.Add(-h.remoteWriteOpts.rejectDuration)
		// The two for loops do the trick of not allocating new memory and do in place drops
		i := 0
		for _, ts := range req.Timeseries {
			j := 0
			var t int64
			for _, s := range ts.Samples {
				t = s.Timestamp
				if time.UnixMilli(s.Timestamp).After(thresholdTime) {
					// preserve samples that are new by reusing the same memory
					ts.Samples[j] = s
					j++
				}
			}
			h.metrics.writeRejectTooOld.Inc(int64(len(ts.Samples) - j))
			if j > 0 {
				ts.Samples = ts.Samples[:j]
				req.Timeseries[i] = ts
				i++
			} else if rand.Float32() < h.remoteWriteOpts.errorSamplingRate {
				h.instrumentOpts.Logger().Error("reject old samples",
					zap.Array("labels", PromLabels{lbls: ts.Labels}), zap.Time("now", now),
					zap.Time("sample timestamp", time.UnixMilli(t)))
			}
		}
		req.Timeseries = req.Timeseries[:i]
	}

	// Check if any of the labels exceed literal length limits and occasionally print them
	// in a log message for debugging purposes.
	maxTagLiteralLength := int(h.tagOptions.MaxTagLiteralLength())
	for _, ts := range req.Timeseries {
		for _, l := range ts.Labels {
			if len(l.Name) > maxTagLiteralLength || len(l.Value) > maxTagLiteralLength {
				h.maybeLogLabelsWithTooLongLiterals(h.instrumentOpts.Logger(), l)
				err := fmt.Errorf("label literal is too long: nameLength=%d, valueLength=%d, maxLength=%d",
					len(l.Name), len(l.Value), maxTagLiteralLength)
				return parseRequestResult{}, err
			}
		}
	}

	return parseRequestResult{
		Request:        &req,
		Options:        opts,
		CompressResult: result,
	}, nil
}

func (h *PromWriteHandler) write(
	ctx context.Context,
	r *prompb.WriteRequest,
	opts ingest.WriteOptions,
) ingest.BatchError {
	iter, err := newPromTSIter(r.Timeseries, h.tagOptions, h.storeMetricsType)
	if err != nil {
		var errs xerrors.MultiError
		return errs.Add(err)
	}
	return h.downsamplerAndWriter.WriteBatch(ctx, iter, opts)
}

func (h *PromWriteHandler) forward(
	ctx context.Context,
	request prometheus.ParsePromCompressedRequestResult,
	header http.Header,
	target handleroptions.PromWriteHandlerForwardTargetOptions,
) error {
	method := target.Method
	if method == "" {
		method = http.MethodPost
	}
	url := target.URL
	req, err := http.NewRequest(method, url, bytes.NewReader(request.CompressedBody))
	if err != nil {
		return err
	}

	// There are multiple headers that impact coordinator behavior on the write
	// (map tags, storage policy, etc.) that we must forward to the target
	// coordinator to guarantee same behavior as the coordinator that originally
	// received the request.
	if header != nil {
		for h := range header {
			if strings.HasPrefix(h, headers.M3HeaderPrefix) {
				req.Header.Add(h, header.Get(h))
			}
			if target.TenantHeader == h {
				req.Header.Add("THANOS-TENANT", header.Get(h))
			}
		}
	}

	if targetHeaders := target.Headers; targetHeaders != nil {
		// If headers set, attach to request.
		for name, value := range targetHeaders {
			req.Header.Add(name, value)
		}
	}

	resp, err := h.forwardHTTPClient.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		response, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			response = []byte(fmt.Sprintf("error reading body: %v", err))
		}
		return fmt.Errorf("expected status code 2XX: actual=%v, method=%v, url=%v, resp=%s",
			resp.StatusCode, method, url, response)
	}

	return nil
}

func (h *PromWriteHandler) maybeLogLabelsWithTooLongLiterals(logger *zap.Logger, label prompb.Label) {
	if atomic.AddUint32(&h.numLiteralIsTooLong, 1) > maxLiteralIsTooLongLogCount {
		return
	}

	safePrefix := func(b []byte, l int) []byte {
		if len(b) <= l {
			return b
		}
		return b[:l]
	}

	logger.Warn("label exceeds literal length limits",
		zap.String("namePrefix", string(safePrefix(label.Name, literalPrefixLength))),
		zap.Int("nameLength", len(label.Name)),
		zap.String("valuePrefix", string(safePrefix(label.Value, literalPrefixLength))),
		zap.Int("valueLength", len(label.Value)),
	)
}

func newPromTSIter(
	timeseries []prompb.TimeSeries,
	tagOpts models.TagOptions,
	storeMetricsType bool,
) (*promTSIter, error) {
	// Construct the tags and datapoints upfront so that if the iterator
	// is reset, we don't have to generate them twice.
	var (
		tags             = make([]models.Tags, 0, len(timeseries))
		datapoints       = make([]ts.Datapoints, 0, len(timeseries))
		seriesAttributes = make([]ts.SeriesAttributes, 0, len(timeseries))
	)

	graphiteTagOpts := tagOpts.SetIDSchemeType(models.TypeGraphite)
	for _, promTS := range timeseries {
		attributes, err := storage.PromTimeSeriesToSeriesAttributes(promTS)
		if err != nil {
			return nil, err
		}

		// Set the tag options based on the incoming source.
		opts := tagOpts
		if attributes.Source == ts.SourceTypeGraphite {
			opts = graphiteTagOpts
		}

		seriesAttributes = append(seriesAttributes, attributes)
		tags = append(tags, storage.PromLabelsToM3Tags(promTS.Labels, opts))
		datapoints = append(datapoints, storage.PromSamplesToM3Datapoints(promTS.Samples))
	}

	return &promTSIter{
		attributes:       seriesAttributes,
		idx:              -1,
		tags:             tags,
		datapoints:       datapoints,
		storeMetricsType: storeMetricsType,
	}, nil
}

type promTSIter struct {
	idx        int
	err        error
	attributes []ts.SeriesAttributes
	tags       []models.Tags
	datapoints []ts.Datapoints
	metadatas  []ts.Metadata
	annotation []byte

	storeMetricsType bool
}

func (i *promTSIter) Next() bool {
	if i.err != nil {
		return false
	}

	i.idx++
	if i.idx >= len(i.tags) {
		return false
	}

	if !i.storeMetricsType {
		return true
	}

	annotationPayload, err := storage.SeriesAttributesToAnnotationPayload(i.attributes[i.idx])
	if err != nil {
		i.err = err
		return false
	}

	i.annotation, err = annotationPayload.Marshal()
	if err != nil {
		i.err = err
		return false
	}

	if len(i.annotation) == 0 {
		i.annotation = nil
	}

	return true
}

func (i *promTSIter) Current() ingest.IterValue {
	if len(i.tags) == 0 || i.idx < 0 || i.idx >= len(i.tags) {
		return defaultValue
	}

	value := ingest.IterValue{
		Tags:       i.tags[i.idx],
		Datapoints: i.datapoints[i.idx],
		Attributes: i.attributes[i.idx],
		Unit:       xtime.Millisecond,
		Annotation: i.annotation,
	}
	if i.idx < len(i.metadatas) {
		value.Metadata = i.metadatas[i.idx]
	}
	return value
}

func (i *promTSIter) Reset() error {
	i.idx = -1
	i.err = nil
	i.annotation = nil

	return nil
}

func (i *promTSIter) Error() error {
	return i.err
}

func (i *promTSIter) SetCurrentMetadata(metadata ts.Metadata) {
	if len(i.metadatas) == 0 {
		i.metadatas = make([]ts.Metadata, len(i.tags))
	}
	if i.idx < 0 || i.idx >= len(i.metadatas) {
		return
	}
	i.metadatas[i.idx] = metadata
}
