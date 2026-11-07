package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRegistrationContract(t *testing.T) {
	out, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("registration = %T", out)
	}
	if reg["schema_version"] != pluginabi.SchemaVersion || pluginabi.SchemaVersion != 10 {
		t.Fatalf("schema = %v", reg["schema_version"])
	}
	meta := reg["metadata"].(pluginapi.Metadata)
	if meta.Name != "OMC WorkBuddy OAuth" || meta.Version != pluginVersion {
		t.Fatalf("metadata = %+v", meta)
	}
	if meta.Logo != publicAssetBaseURL+pluginLogo {
		t.Fatalf("logo = %q", meta.Logo)
	}
	if len(meta.ConfigFields) != 4 {
		t.Fatalf("config fields = %+v", meta.ConfigFields)
	}
	for _, field := range meta.ConfigFields {
		if field.Default == nil || field.Description == "" {
			t.Fatalf("field %q has no default or description", field.Name)
		}
	}
	caps := reg["capabilities"].(map[string]any)
	if caps["executor_input_formats"].([]string)[0] != "openai" || caps["executor_output_formats"].([]string)[0] != "openai" {
		t.Fatalf("formats = %v", caps)
	}
	if caps["executor_model_scope"] != "oauth" {
		t.Fatalf("scope = %v", caps["executor_model_scope"])
	}
}

func TestAuthIdentifierPublishesTwoEntries(t *testing.T) {
	out, err := handleMethod("auth.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(map[string]any)
	if got["identifier"] != credentialProvider || got["credential_providers"].([]string)[0] != credentialProvider {
		t.Fatalf("identifier = %v", got)
	}
	ids := got["identifiers"].([]string)
	if len(ids) != 2 || ids[0] != siteWorkBuddy || ids[1] != siteWorkBuddyAI {
		t.Fatalf("identifiers = %v", ids)
	}
	entries := got["entries"].([]pluginapi.AuthProviderEntry)
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	if entries[0].Identifier != siteWorkBuddy || entries[0].Name != "WorkBuddy" || entries[1].Name != "WorkBuddy AI" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Logo == entries[1].Logo {
		t.Fatal("the two entries share one logo")
	}
	if entries[0].Logo != publicAssetBaseURL+"workbuddy.svg" || entries[1].Logo != publicAssetBaseURL+"codebuddy-color.svg" {
		t.Fatalf("logos = %q %q", entries[0].Logo, entries[1].Logo)
	}
	for _, method := range []string{"executor.identifier", "quota.identifier"} {
		out, err := handleMethod(method, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out.(map[string]string)["identifier"] != credentialProvider {
			t.Fatalf("%s = %v", method, out)
		}
	}
}

func TestParseAuthReadsPersistedFile(t *testing.T) {
	raw := map[string]any{
		"type":            "workbuddy",
		"site":            siteWorkBuddyAI,
		"access":          "at",
		"refresh":         "rt",
		"expires":         1900000000000,
		"uid":             "u-1",
		"name":            "Ada",
		"enterprise_id":   "ent-1",
		"enterprise_name": "Acme",
		"oauth_provider":  siteWorkBuddyAI,
	}
	body, _ := json.Marshal(raw)
	req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: siteWorkBuddyAI, FileName: "workbuddy-x.json", RawJSON: body})
	out, err := handleMethod("auth.parse", req)
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(pluginapi.AuthParseResponse)
	if !resp.Handled || resp.Auth.Provider != credentialProvider {
		t.Fatalf("parse = %+v", resp)
	}
	if resp.Auth.Metadata["oauth_provider"] != siteWorkBuddyAI {
		t.Fatalf("auth = %+v", resp.Auth)
	}
	if resp.Auth.Label != "Ada · Acme" {
		t.Fatalf("label = %q", resp.Auth.Label)
	}
	if resp.Auth.ID != "workbuddy-x.json" || resp.Auth.FileName != "workbuddy-x.json" {
		t.Fatalf("identity = %q %q", resp.Auth.ID, resp.Auth.FileName)
	}
}

func TestParseAuthRejectsForeignFiles(t *testing.T) {
	cases := []string{
		`{"type":"qoder","site":"workbuddy","uid":"u","access":"a"}`,
		`{"type":"workbuddy","site":"elsewhere","uid":"u","access":"a"}`,
		`{"type":"workbuddy","site":"workbuddy","uid":"u"}`,
		`{"type":"workbuddy","site":"workbuddy","access":"a"}`,
	}
	for _, body := range cases {
		req, _ := json.Marshal(pluginapi.AuthParseRequest{RawJSON: []byte(body)})
		out, err := handleMethod("auth.parse", req)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if out.(pluginapi.AuthParseResponse).Handled {
			t.Fatalf("%s was accepted", body)
		}
	}
	req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: "qoder", RawJSON: []byte(`{"type":"workbuddy","site":"workbuddy","uid":"u","access":"a"}`)})
	out, err := handleMethod("auth.parse", req)
	if err != nil {
		t.Fatal(err)
	}
	if out.(pluginapi.AuthParseResponse).Handled {
		t.Fatal("a qoder login entry claimed a WorkBuddy auth file")
	}
}

func TestManagementServesPublishedBytes(t *testing.T) {
	for _, name := range []string{"workbuddy.svg", "codebuddy-color.svg"} {
		want, err := os.ReadFile("../assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := handleMethod("management.handle", mustJSON(t, map[string]any{
			"Method": http.MethodGet,
			"Path":   resourcePrefix + "/" + name,
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := resp.(pluginapi.ManagementResponse)
		if got.StatusCode != http.StatusOK || !bytes.Equal(got.Body, want) {
			t.Fatalf("%s: status %d, %d bytes", name, got.StatusCode, len(got.Body))
		}
		if got.Headers.Get("Content-Type") != "image/svg+xml" || got.Headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s headers = %v", name, got.Headers)
		}
	}
	resp, _ := handleMethod("management.handle", mustJSON(t, map[string]any{"Method": http.MethodGet, "Path": omcResourcePath}))
	descriptor := resp.(pluginapi.ManagementResponse)
	if descriptor.Headers.Get("Content-Type") != "application/json; charset=utf-8" || string(descriptor.Body) != omcDescriptorJSON {
		t.Fatal("the declaration is not served as the bytes the plugin embeds")
	}
	var document map[string]any
	if err := json.Unmarshal(descriptor.Body, &document); err != nil {
		t.Fatal(err)
	}
	if document["plugin_id"] != pluginStoreID || document["plugin_version"] != pluginVersion {
		t.Fatalf("declaration = %v", document)
	}
}

func TestManagementRejectsWritesAndUnknownPaths(t *testing.T) {
	resp, _ := handleMethod("management.handle", mustJSON(t, map[string]any{"Method": http.MethodPost, "Path": resourcePrefix + "/workbuddy.svg"}))
	if got := resp.(pluginapi.ManagementResponse); got.StatusCode != http.StatusMethodNotAllowed || got.Headers.Get("Allow") != http.MethodGet {
		t.Fatalf("write = %+v", got)
	}
	resp, _ = handleMethod("management.handle", mustJSON(t, map[string]any{"Method": http.MethodGet, "Path": resourcePrefix + "/other.svg"}))
	if got := resp.(pluginapi.ManagementResponse); got.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path = %+v", got)
	}
}

func TestEmbeddedAssetsMatchPublishedOnes(t *testing.T) {
	// The declaration is authored where the plugin embeds it; the two marks are
	// published from assets/ and embedded from go/assets/.
	for _, name := range []string{"workbuddy.svg", "codebuddy-color.svg"} {
		published, err := os.ReadFile("../assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := os.ReadFile("assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(published, embedded) {
			t.Fatalf("%s differs between assets/ and go/assets/", name)
		}
	}
}

func TestConfigurationValidation(t *testing.T) {
	valid := []string{
		"",
		"site: workbuddy-ai",
		"site: workbuddy\nmax_concurrency: 5",
		"app_version: 5.10.0\ncli_version: 2.200.0",
	}
	for _, raw := range valid {
		if err := configure([]byte(raw)); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
	}
	invalid := []string{
		"site: qoder",
		"max_concurrency: 0",
		"max_concurrency: 101",
		"app_version: \"\"",
		"cli_version: 2.165.0 ; rm -rf /",
	}
	for _, raw := range invalid {
		if err := configure([]byte(raw)); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
