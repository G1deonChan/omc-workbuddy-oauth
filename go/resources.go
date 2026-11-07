package main

import (
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pluginStoreID = "omc-workbuddy-oauth"
const resourcePrefix = "/v0/resource/plugins/" + pluginStoreID

// publicAssetBaseURL is the published location of the same artwork the resource
// routes serve. OAuth entry logos use it because a panel reads a plugin logo as a
// document, and the store manifests are URLs too.
const publicAssetBaseURL = "https://raw.githubusercontent.com/omc-community/omc-workbuddy-oauth/refs/heads/main/assets/"

// pluginLogo is WorkBuddy's own mark: the plugin's face in a store, and the sign-in
// card of the China build. The international build's card carries CodeBuddy's mark,
// because that build's own assets are served from CodeBuddy's hosts.
const pluginLogo = "workbuddy.svg"

// aiLogo is the international build's card.
const aiLogo = "codebuddy-color.svg"

//go:embed assets/workbuddy.svg
var workbuddyLogo []byte

//go:embed assets/codebuddy-color.svg
var codebuddyLogo []byte

// resourceAssetURL is the public address of one embedded mark. Only the two names
// registered below are addressable, so the set of URLs the plugin can publish stays
// equal to the set of bytes it ships.
func resourceAssetURL(name string) string {
	switch name {
	case pluginLogo, aiLogo:
		return publicAssetBaseURL + name
	default:
		return ""
	}
}

// handleManagementResource serves only deployed public bytes. It deliberately has
// no host callback, auth directory, configuration or OAuth-state dependency.
func handleManagementResource(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	var body []byte
	contentType := "image/svg+xml"
	cacheControl := "public, max-age=86400"
	switch req.Path {
	case resourcePrefix + "/" + pluginLogo:
		body = workbuddyLogo
	case resourcePrefix + "/" + aiLogo:
		body = codebuddyLogo
	case omcResourcePath:
		body = []byte(omcDescriptorJSON)
		contentType = "application/json; charset=utf-8"
		cacheControl = "no-cache"
	default:
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte("not found")}, nil
	}
	if req.Method != http.MethodGet {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    http.Header{"Allow": {http.MethodGet}},
			Body:       []byte("method not allowed"),
		}, nil
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {contentType},
			"Cache-Control":           {cacheControl},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'"},
		},
		Body: append([]byte(nil), body...),
	}, nil
}
