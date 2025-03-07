// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ecsutil // import "github.com/open-telemetry/opentelemetry-collector-contrib/internal/aws/ecsutil"

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	"go.opentelemetry.io/collector/component"
	cconfig "go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal/sanitize"
)

// Client defines the basic HTTP client interface with GET response validation and content parsing
type Client interface {
	Get(path string) ([]byte, error)
}

// NewClientProvider creates the default rest client provider
func NewClientProvider(baseURL url.URL, clientSettings confighttp.HTTPClientSettings, logger *zap.Logger) ClientProvider {
	return &defaultClientProvider{
		baseURL:        baseURL,
		clientSettings: clientSettings,
		logger:         logger,
	}
}

// ClientProvider defines
type ClientProvider interface {
	BuildClient() (Client, error)
}

type defaultClientProvider struct {
	baseURL        url.URL
	clientSettings confighttp.HTTPClientSettings
	logger         *zap.Logger
}

func (dcp *defaultClientProvider) BuildClient() (Client, error) {
	return defaultClient(
		dcp.baseURL,
		dcp.clientSettings,
		dcp.logger,
	)
}

func defaultClient(
	baseURL url.URL,
	clientSettings confighttp.HTTPClientSettings,
	logger *zap.Logger,
) (*clientImpl, error) {
	client, err := clientSettings.ToClient(map[cconfig.ComponentID]component.Extension{})
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("unexpected default client nil value")
	}
	return &clientImpl{
		baseURL:    baseURL,
		httpClient: *client,
		logger:     logger,
	}, nil
}

var _ Client = (*clientImpl)(nil)

type clientImpl struct {
	baseURL    url.URL
	httpClient http.Client
	logger     *zap.Logger
}

func (c *clientImpl) Get(path string) ([]byte, error) {
	req, err := c.buildReq(path)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeErr := resp.Body.Close()
		if closeErr != nil {
			c.logger.Warn("Failed to close response body", zap.Error(closeErr))
		}
	}()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request GET %s failed - %q", sanitize.URL(req.URL), resp.Status)
	}
	return body, nil
}

func (c *clientImpl) buildReq(path string) (*http.Request, error) {
	url := c.baseURL.String() + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}
