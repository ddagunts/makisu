//  Copyright (c) 2018 Uber Technologies, Inc.
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

package security

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/distribution/registry/client/auth"
	"github.com/docker/distribution/registry/client/auth/challenge"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/engine-api/types"
	"github.com/uber/makisu/lib/utils/httputil"
)

const (
	basePingQuery         = "https://%s/v2/"
	registryVersionHeader = "Docker-Distribution-Api-Version"
)

var v2Version = auth.APIVersion{
	Type:    "registry",
	Version: "2.0",
}

// BasicAuthTransport creates a transport that does basic authentication.
func BasicAuthTransport(addr, repo string, tr http.RoundTripper, authConfig types.AuthConfig) (http.RoundTripper, error) {
	cm, err := ping(addr, tr)
	if err != nil {
		return nil, fmt.Errorf("ping v2 registry: %s", err)
	}
	// This looks weird but when using AWS ECR (especially the docker ecr helper) we get a Username and a Password
	// Then, the ping will create a challenge by parsing the www-authenticate header from the ECR server (it will return a "Basic ...")
	// So if we use the `NewTokenHandlerWithOptions` we will always fail the Scheme checking in vendor/github.com/docker/distribution/registry/client/auth/session.go#L98 ("basic" != "bearer")
	if authConfig.Username != "" && authConfig.Password != "" && strings.HasSuffix(addr, "amazonaws.com") {
		return transport.NewTransport(tr, auth.NewAuthorizer(cm, auth.NewBasicHandler(defaultCredStore{authConfig}))), nil
	} else {
		return transport.NewTransport(tr, auth.NewAuthorizer(cm, auth.NewTokenHandlerWithOptions(auth.TokenHandlerOptions{
			Transport:   tr,
			Credentials: defaultCredStore{authConfig},
			Scopes: []auth.Scope{
				auth.RepositoryScope{
					Repository: repo,
					Actions:    []string{"pull", "push"},
				},
			},
			ClientID:   "docker",
			ForceOAuth: false, // Only support basic auth.
		}))), nil
	}
}

func ping(addr string, tr http.RoundTripper) (challenge.Manager, error) {
	resp, err := httputil.Send(
		"GET",
		fmt.Sprintf(basePingQuery, addr),
		httputil.SendTLSTransport(tr),
		httputil.SendAcceptedCodes(http.StatusOK, http.StatusUnauthorized),
	)
	if err != nil {
		return nil, err
	}

	versions := auth.APIVersions(resp, registryVersionHeader)
	for _, version := range versions {
		if version == v2Version {
			cm := challenge.NewSimpleManager()
			if err := cm.AddResponse(resp); err != nil {
				return nil, fmt.Errorf("add response: %s", err)
			}
			return cm, nil
		}
	}
	return nil, fmt.Errorf("registry is not v2")
}

type defaultCredStore struct {
	config types.AuthConfig
}

func (scs defaultCredStore) Basic(*url.URL) (string, string) {
	return scs.config.Username, scs.config.Password
}

func (scs defaultCredStore) RefreshToken(*url.URL, string) string {
	return scs.config.IdentityToken
}

func (scs defaultCredStore) SetRefreshToken(*url.URL, string, string) {}
