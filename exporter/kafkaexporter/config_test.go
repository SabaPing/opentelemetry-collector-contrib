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

package kafkaexporter

import (
	"path"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/service/servicetest"
)

func TestLoadConfig(t *testing.T) {
	factories, err := componenttest.NopFactories()
	assert.NoError(t, err)

	factory := NewFactory()
	factories.Exporters[typeStr] = factory
	cfg, err := servicetest.LoadConfigAndValidate(path.Join(".", "testdata", "config.yaml"), factories)
	require.NoError(t, err)
	require.Equal(t, 1, len(cfg.Exporters))

	c := cfg.Exporters[config.NewComponentID(typeStr)].(*Config)
	assert.Equal(t, &Config{
		ExporterSettings: config.NewExporterSettings(config.NewComponentID(typeStr)),
		TimeoutSettings: exporterhelper.TimeoutSettings{
			Timeout: 10 * time.Second,
		},
		RetrySettings: exporterhelper.RetrySettings{
			Enabled:         true,
			InitialInterval: 10 * time.Second,
			MaxInterval:     1 * time.Minute,
			MaxElapsedTime:  10 * time.Minute,
		},
		QueueSettings: exporterhelper.QueueSettings{
			Enabled:      true,
			NumConsumers: 2,
			QueueSize:    10,
		},
		Topic:    "spans",
		Encoding: "otlp_proto",
		Brokers:  []string{"foo:123", "bar:456"},
		Authentication: Authentication{
			PlainText: &PlainTextConfig{
				Username: "jdoe",
				Password: "pass",
			},
		},
		Metadata: Metadata{
			Full: false,
			Retry: MetadataRetry{
				Max:     15,
				Backoff: defaultMetadataRetryBackoff,
			},
		},
		Producer: Producer{
			MaxMessageBytes: 10000000,
			RequiredAcks:    sarama.WaitForAll,
		},
	}, c)
}
