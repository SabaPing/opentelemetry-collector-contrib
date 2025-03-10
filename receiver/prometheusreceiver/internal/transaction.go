// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/prometheusreceiver/internal"

import (
	"context"
	"errors"
	"net"
	"sync/atomic"

	commonpb "github.com/census-instrumentation/opencensus-proto/gen-go/agent/common/v1"
	metricspb "github.com/census-instrumentation/opencensus-proto/gen-go/metrics/v1"
	resourcepb "github.com/census-instrumentation/opencensus-proto/gen-go/resource/v1"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/model/pdata"
	"go.opentelemetry.io/collector/obsreport"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/opencensus"
)

const (
	portAttr     = "port"
	schemeAttr   = "scheme"
	jobAttr      = "job"
	instanceAttr = "instance"

	transport  = "http"
	dataformat = "prometheus"
)

var errMetricNameNotFound = errors.New("metricName not found from labels")
var errTransactionAborted = errors.New("transaction aborted")
var errNoJobInstance = errors.New("job or instance cannot be found from labels")
var errNoStartTimeMetrics = errors.New("process_start_time_seconds metric is missing")

// A transaction is corresponding to an individual scrape operation or stale report.
// That said, whenever prometheus receiver scrapped a target metric endpoint a page of raw metrics is returned,
// a transaction, which acts as appender, is created to process this page of data, the scrapeLoop will call the Add or
// AddFast method to insert metrics data points, when finished either Commit, which means success, is called and data
// will be flush to the downstream consumer, or Rollback, which means discard all the data, is called and all data
// points are discarded.
type transaction struct {
	id                   int64
	ctx                  context.Context
	isNew                bool
	sink                 consumer.Metrics
	job                  string
	instance             string
	jobsMap              *JobsMapPdata
	useStartTimeMetric   bool
	startTimeMetricRegex string
	ms                   *metadataService
	node                 *commonpb.Node
	resource             *resourcepb.Resource
	metricBuilder        *metricBuilder
	externalLabels       labels.Labels
	logger               *zap.Logger
	obsrecv              *obsreport.Receiver
	startTimeMs          int64
}

func newTransaction(
	ctx context.Context,
	jobsMap *JobsMapPdata,
	useStartTimeMetric bool,
	startTimeMetricRegex string,
	receiverID config.ComponentID,
	ms *metadataService,
	sink consumer.Metrics,
	externalLabels labels.Labels,
	set component.ReceiverCreateSettings) *transaction {
	return &transaction{
		id:                   atomic.AddInt64(&idSeq, 1),
		ctx:                  ctx,
		isNew:                true,
		sink:                 sink,
		jobsMap:              jobsMap,
		useStartTimeMetric:   useStartTimeMetric,
		startTimeMetricRegex: startTimeMetricRegex,
		ms:                   ms,
		externalLabels:       externalLabels,
		logger:               set.Logger,
		obsrecv: obsreport.NewReceiver(obsreport.ReceiverSettings{
			ReceiverID:             receiverID,
			Transport:              transport,
			ReceiverCreateSettings: set,
		}),
		startTimeMs: -1,
	}
}

// ensure *transaction has implemented the storage.Appender interface
var _ storage.Appender = (*transaction)(nil)

// Append always returns 0 to disable label caching.
func (tr *transaction) Append(ref storage.SeriesRef, ls labels.Labels, t int64, v float64) (storage.SeriesRef, error) {
	if tr.startTimeMs < 0 {
		tr.startTimeMs = t
	}

	select {
	case <-tr.ctx.Done():
		return 0, errTransactionAborted
	default:
	}
	if len(tr.externalLabels) > 0 {
		// TODO(jbd): Improve the allocs.
		ls = append(ls, tr.externalLabels...)
	}
	if tr.isNew {
		if err := tr.initTransaction(ls); err != nil {
			return 0, err
		}
	}
	return 0, tr.metricBuilder.AddDataPoint(ls, t, v)
}

func (tr *transaction) AppendExemplar(ref storage.SeriesRef, l labels.Labels, e exemplar.Exemplar) (storage.SeriesRef, error) {
	return 0, nil
}

// AddFast always returns error since caching is not supported by Add() function.
func (tr *transaction) AddFast(_ uint64, _ int64, _ float64) error {
	return storage.ErrNotFound
}

func (tr *transaction) initTransaction(ls labels.Labels) error {
	job, instance := ls.Get(model.JobLabel), ls.Get(model.InstanceLabel)
	if job == "" || instance == "" {
		return errNoJobInstance
	}
	// discover the binding target when this method is called for the first time during a transaction
	mc, err := tr.ms.Get(job, instance)
	if err != nil {
		return err
	}
	if tr.jobsMap != nil {
		tr.job = job
		tr.instance = instance
	}
	tr.node, tr.resource = createNodeAndResource(job, instance, mc.SharedLabels().Get(model.SchemeLabel))
	tr.metricBuilder = newMetricBuilder(mc, tr.useStartTimeMetric, tr.startTimeMetricRegex, tr.logger, tr.startTimeMs)
	tr.isNew = false
	return nil
}

// Commit submits metrics data to consumers.
func (tr *transaction) Commit() error {
	if tr.isNew {
		// In a situation like not able to connect to the remote server, scrapeloop will still commit even if it had
		// never added any data points, that the transaction has not been initialized.
		return nil
	}

	tr.startTimeMs = -1

	ctx := tr.obsrecv.StartMetricsOp(tr.ctx)
	metrics, _, _, err := tr.metricBuilder.Build()
	if err != nil {
		// Only error by Build() is errNoDataToBuild, with numReceivedPoints set to zero.
		tr.obsrecv.EndMetricsOp(ctx, dataformat, 0, err)
		return err
	}

	if tr.useStartTimeMetric {
		// startTime is mandatory in this case, but may be zero when the
		// process_start_time_seconds metric is missing from the target endpoint.
		if tr.metricBuilder.startTime == 0.0 {
			// Since we are unable to adjust metrics properly, we will drop them
			// and return an error.
			err = errNoStartTimeMetrics
			tr.obsrecv.EndMetricsOp(ctx, dataformat, 0, err)
			return err
		}

		adjustStartTimestamp(tr.metricBuilder.startTime, metrics)
	}

	numPoints := 0
	var md pdata.Metrics
	if len(metrics) > 0 {
		md = opencensus.OCToMetrics(tr.node, tr.resource, metrics)
		fixStaleMetrics(&md)
		numPoints = md.DataPointCount()
	}

	if !tr.useStartTimeMetric {
		_ = NewMetricsAdjusterPdata(tr.jobsMap.get(tr.job, tr.instance), tr.logger).AdjustMetrics(&md)
	}

	if numPoints > 0 {
		err = tr.sink.ConsumeMetrics(ctx, md)
	}
	tr.obsrecv.EndMetricsOp(ctx, dataformat, numPoints, err)
	return err
}

func (tr *transaction) Rollback() error {
	tr.startTimeMs = -1
	return nil
}

func adjustStartTimestamp(startTime float64, metrics []*metricspb.Metric) {
	startTimeTs := timestampFromFloat64(startTime)
	for _, metric := range metrics {
		switch metric.GetMetricDescriptor().GetType() {
		case metricspb.MetricDescriptor_GAUGE_DOUBLE, metricspb.MetricDescriptor_GAUGE_DISTRIBUTION:
			continue
		default:
			for _, ts := range metric.GetTimeseries() {
				ts.StartTimestamp = startTimeTs
			}
		}
	}
}

func timestampFromFloat64(ts float64) *timestamppb.Timestamp {
	secs := int64(ts)
	nanos := int64((ts - float64(secs)) * 1e9)
	return &timestamppb.Timestamp{
		Seconds: secs,
		Nanos:   int32(nanos),
	}
}

func createNodeAndResource(job, instance, scheme string) (*commonpb.Node, *resourcepb.Resource) {
	host, port, err := net.SplitHostPort(instance)
	if err != nil {
		host = instance
	}

	node := &commonpb.Node{
		ServiceInfo: &commonpb.ServiceInfo{Name: job},
	}

	if isDiscernibleHost(host) {
		node.Identifier = &commonpb.ProcessIdentifier{
			HostName: host,
		}
	}

	resource := &resourcepb.Resource{
		Labels: map[string]string{
			jobAttr:      job,
			instanceAttr: instance,
			portAttr:     port,
			schemeAttr:   scheme,
		},
	}
	return node, resource
}

func fixStaleMetrics(md *pdata.Metrics) {
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		for j := 0; j < rm.InstrumentationLibraryMetrics().Len(); j++ {
			ilm := rm.InstrumentationLibraryMetrics().At(j)
			for k := 0; k < ilm.Metrics().Len(); k++ {
				metric := ilm.Metrics().At(k)
				switch metric.DataType() {
				case pdata.MetricDataTypeHistogram:
					fixStaleHistogram(metric.Histogram())
				case pdata.MetricDataTypeSummary:
					fixStaleSummary(metric.Summary())
				case pdata.MetricDataTypeSum:
					fixStaleSum(metric.Sum())
				case pdata.MetricDataTypeGauge:
					fixStaleGauge(metric.Gauge())
				}
			}
		}
	}
}

func fixStaleHistogram(hist pdata.Histogram) {
	for i := 0; i < hist.DataPoints().Len(); i++ {
		dp := hist.DataPoints().At(i)
		if value.IsStaleNaN(dp.Sum()) {
			dp.SetFlags(pdataStaleFlags)
			dp.SetCount(0)
			dp.SetSum(0)
			dp.SetBucketCounts([]uint64{})
			dp.SetExplicitBounds([]float64{})
		}
	}
}

func fixStaleSummary(sum pdata.Summary) {
	for i := 0; i < sum.DataPoints().Len(); i++ {
		dp := sum.DataPoints().At(i)
		if value.IsStaleNaN(dp.Sum()) {
			dp.SetFlags(pdataStaleFlags)
			dp.SetCount(0)
			dp.SetSum(0)
			dp.QuantileValues().RemoveIf(func(_ pdata.ValueAtQuantile) bool {
				return true
			})
		}
	}
}

func fixStaleSum(sum pdata.Sum) {
	for i := 0; i < sum.DataPoints().Len(); i++ {
		dp := sum.DataPoints().At(i)
		if value.IsStaleNaN(dp.DoubleVal()) {
			dp.SetFlags(pdataStaleFlags)
			dp.SetDoubleVal(0)
		}
	}
}

func fixStaleGauge(gauge pdata.Gauge) {
	for i := 0; i < gauge.DataPoints().Len(); i++ {
		dp := gauge.DataPoints().At(i)
		if value.IsStaleNaN(dp.DoubleVal()) {
			dp.SetFlags(pdataStaleFlags)
			dp.SetDoubleVal(0)
		}
	}
}
