// Copyright  The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package elasticsearchreceiver

import (
	"context"
	"encoding/json"
	"errors"
	"io/ioutil"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/model/pdata"
	"go.opentelemetry.io/collector/receiver/scrapererror"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/scrapertest"
	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/scrapertest/golden"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/elasticsearchreceiver/internal/mocks"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/elasticsearchreceiver/internal/model"
)

const fullExpectedMetricsPath = "./testdata/expected_metrics/full.json"
const skipClusterExpectedMetricsPath = "./testdata/expected_metrics/clusterSkip.json"
const noNodesExpectedMetricsPath = "./testdata/expected_metrics/noNodes.json"

func TestScraper(t *testing.T) {
	t.Parallel()

	sc := newElasticSearchScraper(zap.NewNop(), createDefaultConfig().(*Config))

	err := sc.start(context.Background(), componenttest.NewNopHost())
	require.NoError(t, err)

	mockClient := mocks.MockElasticsearchClient{}
	mockClient.On("ClusterHealth", mock.Anything).Return(clusterHealth(t), nil)
	mockClient.On("NodeStats", mock.Anything, []string{"_all"}).Return(nodeStats(t), nil)

	sc.client = &mockClient

	expectedMetrics, err := golden.ReadMetrics(fullExpectedMetricsPath)
	require.NoError(t, err)

	actualMetrics, err := sc.scrape(context.Background())
	require.NoError(t, err)

	requireMetricsEqual(t, expectedMetrics, actualMetrics)
}

func TestScraperSkipClusterMetrics(t *testing.T) {
	t.Parallel()

	conf := createDefaultConfig().(*Config)
	conf.SkipClusterMetrics = true

	sc := newElasticSearchScraper(zap.NewNop(), conf)

	err := sc.start(context.Background(), componenttest.NewNopHost())
	require.NoError(t, err)

	mockClient := mocks.MockElasticsearchClient{}
	mockClient.On("ClusterHealth", mock.Anything).Return(clusterHealth(t), nil)
	mockClient.On("NodeStats", mock.Anything, []string{"_all"}).Return(nodeStats(t), nil)

	sc.client = &mockClient

	expectedMetrics, err := golden.ReadMetrics(skipClusterExpectedMetricsPath)
	require.NoError(t, err)

	actualMetrics, err := sc.scrape(context.Background())
	require.NoError(t, err)

	requireMetricsEqual(t, expectedMetrics, actualMetrics)
}

func TestScraperNoNodesMetrics(t *testing.T) {
	t.Parallel()

	conf := createDefaultConfig().(*Config)
	conf.Nodes = []string{}

	sc := newElasticSearchScraper(zap.NewNop(), conf)

	err := sc.start(context.Background(), componenttest.NewNopHost())
	require.NoError(t, err)

	mockClient := mocks.MockElasticsearchClient{}
	mockClient.On("ClusterHealth", mock.Anything).Return(clusterHealth(t), nil)
	mockClient.On("NodeStats", mock.Anything, []string{}).Return(nodeStats(t), nil)

	sc.client = &mockClient

	expectedMetrics, err := golden.ReadMetrics(noNodesExpectedMetricsPath)
	require.NoError(t, err)

	actualMetrics, err := sc.scrape(context.Background())
	require.NoError(t, err)

	requireMetricsEqual(t, expectedMetrics, actualMetrics)
}

func TestScraperFailedStart(t *testing.T) {
	t.Parallel()

	conf := createDefaultConfig().(*Config)

	conf.HTTPClientSettings = confighttp.HTTPClientSettings{
		Endpoint: "localhost:9200",
		TLSSetting: configtls.TLSClientSetting{
			TLSSetting: configtls.TLSSetting{
				CAFile: "/non/existent",
			},
		},
	}

	conf.Username = "dev"
	conf.Password = "dev"

	sc := newElasticSearchScraper(zap.NewNop(), conf)

	err := sc.start(context.Background(), componenttest.NewNopHost())
	require.Error(t, err)
}

func TestScrapingError(t *testing.T) {
	testCases := []struct {
		desc string
		run  func(t *testing.T)
	}{
		{
			desc: "Node stats fails, but cluster health succeeds",
			run: func(t *testing.T) {
				t.Parallel()

				err404 := errors.New("expected status 200 but got 404")

				mockClient := mocks.MockElasticsearchClient{}
				mockClient.On("NodeStats", mock.Anything, []string{"_all"}).Return(nil, err404)
				mockClient.On("ClusterHealth", mock.Anything).Return(clusterHealth(t), nil)

				sc := newElasticSearchScraper(zap.NewNop(), createDefaultConfig().(*Config))
				err := sc.start(context.Background(), componenttest.NewNopHost())
				require.NoError(t, err)

				sc.client = &mockClient

				_, err = sc.scrape(context.Background())
				require.True(t, scrapererror.IsPartialScrapeError(err))
				require.Equal(t, err.Error(), err404.Error())

			},
		},
		{
			desc: "Cluster health fails, but node stats succeeds",
			run: func(t *testing.T) {
				t.Parallel()

				err404 := errors.New("expected status 200 but got 404")

				mockClient := mocks.MockElasticsearchClient{}
				mockClient.On("NodeStats", mock.Anything, []string{"_all"}).Return(nodeStats(t), nil)
				mockClient.On("ClusterHealth", mock.Anything).Return(nil, err404)

				sc := newElasticSearchScraper(zap.NewNop(), createDefaultConfig().(*Config))
				err := sc.start(context.Background(), componenttest.NewNopHost())
				require.NoError(t, err)

				sc.client = &mockClient

				_, err = sc.scrape(context.Background())
				require.True(t, scrapererror.IsPartialScrapeError(err))
				require.Equal(t, err.Error(), err404.Error())

			},
		},
		{
			desc: "Both node stats and cluster health fails",
			run: func(t *testing.T) {
				t.Parallel()

				err404 := errors.New("expected status 200 but got 404")
				err500 := errors.New("expected status 200 but got 500")

				mockClient := mocks.MockElasticsearchClient{}
				mockClient.On("NodeStats", mock.Anything, []string{"_all"}).Return(nil, err500)
				mockClient.On("ClusterHealth", mock.Anything).Return(nil, err404)

				sc := newElasticSearchScraper(zap.NewNop(), createDefaultConfig().(*Config))
				err := sc.start(context.Background(), componenttest.NewNopHost())
				require.NoError(t, err)

				sc.client = &mockClient

				m, err := sc.scrape(context.Background())
				require.Contains(t, err.Error(), err404.Error())
				require.Contains(t, err.Error(), err500.Error())

				require.Equal(t, m.DataPointCount(), 0)
			},
		},
		{
			desc: "Cluster health status is invalid",
			run: func(t *testing.T) {
				t.Parallel()

				ch := clusterHealth(t)
				ch.Status = "pink"

				mockClient := mocks.MockElasticsearchClient{}
				mockClient.On("NodeStats", mock.Anything, []string{"_all"}).Return(nodeStats(t), nil)
				mockClient.On("ClusterHealth", mock.Anything).Return(ch, nil)

				sc := newElasticSearchScraper(zap.NewNop(), createDefaultConfig().(*Config))
				err := sc.start(context.Background(), componenttest.NewNopHost())
				require.NoError(t, err)

				sc.client = &mockClient

				_, err = sc.scrape(context.Background())
				require.True(t, scrapererror.IsPartialScrapeError(err))
				require.Contains(t, err.Error(), errUnknownClusterStatus.Error())
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.desc, testCase.run)
	}
}

func clusterHealth(t *testing.T) *model.ClusterHealth {
	healthJSON, err := ioutil.ReadFile("./testdata/sample_payloads/health.json")
	require.NoError(t, err)

	clusterHealth := model.ClusterHealth{}
	require.NoError(t, json.Unmarshal(healthJSON, &clusterHealth))

	return &clusterHealth
}

func nodeStats(t *testing.T) *model.NodeStats {
	nodeJSON, err := ioutil.ReadFile("./testdata/sample_payloads/nodes_linux.json")
	require.NoError(t, err)

	nodeStats := model.NodeStats{}
	require.NoError(t, json.Unmarshal(nodeJSON, &nodeStats))
	return &nodeStats
}

func requireMetricsEqual(t *testing.T, m1, m2 pdata.Metrics) {
	rms1 := m1.ResourceMetrics()
	rms2 := m2.ResourceMetrics()
	if rms1.Len() != rms2.Len() {
		require.Fail(t, "First metric had %d resource metrics, second had %d", rms1.Len(), rms2.Len())
	}

	for i := 0; i < rms1.Len(); i++ {
		rm1 := rms1.At(i)
		rm2 := rms2.At(i)

		require.Equal(t, rm1.Resource().Attributes().AsRaw(), rm2.Resource().Attributes().AsRaw())

		ilms1 := rm1.InstrumentationLibraryMetrics()
		ilms2 := rm2.InstrumentationLibraryMetrics()

		if ilms1.Len() != ilms2.Len() {
			require.FailNow(t, "Resource metric %d: First metric had %d InstrumentationLibrary metrics, second had %d", i, ilms1.Len(), ilms2.Len())
		}

		for j := 0; j < ilms1.Len(); j++ {
			ilm1 := ilms1.At(j)
			ilm2 := ilms2.At(j)

			require.Equal(t, ilm1.InstrumentationLibrary().Name(), ilm2.InstrumentationLibrary().Name())

			err := scrapertest.CompareMetricSlices(ilm1.Metrics(), ilm2.Metrics())
			require.NoError(t, err)
		}
	}
}
