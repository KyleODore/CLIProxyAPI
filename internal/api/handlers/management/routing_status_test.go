package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetRoutingStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.SoonestResetSelector{}, nil)
	credential, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "routing-status-credential-id",
		Provider: "claude",
		Metadata: map[string]any{
			"access_token":  "routing-status-access-token-secret",
			"refresh_token": "routing-status-refresh-token-secret",
		},
		Attributes: map[string]string{
			coreauth.AttributeAPIKey: "routing-status-api-key-secret",
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	wantAuthIndex := credential.Clone().EnsureIndex()
	handler := NewHandler(&config.Config{}, "", manager)
	router := gin.New()
	router.GET("/routing/status", handler.GetRoutingStatus)

	request := func() (int, string, coreauth.RoutingStatusSnapshot) {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/routing/status", nil))
		var snapshot coreauth.RoutingStatusSnapshot
		if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &snapshot); errUnmarshal != nil {
			t.Fatalf("json.Unmarshal() error = %v; body = %s", errUnmarshal, recorder.Body.String())
		}
		return recorder.Code, recorder.Body.String(), snapshot
	}

	status, body, snapshot := request()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", status, http.StatusOK, body)
	}
	if snapshot.Strategy != "soonest-reset" {
		t.Errorf("strategy = %q, want soonest-reset", snapshot.Strategy)
	}
	if snapshot.Providers == nil {
		t.Fatal("providers is null, want a non-nil map")
	}
	accounts := snapshot.Providers["claude"]
	if len(accounts) != 1 {
		t.Fatalf("claude account count = %d, want 1; body = %s", len(accounts), body)
	}
	if accounts[0].AuthIndex == "" || accounts[0].AuthIndex != wantAuthIndex {
		t.Errorf("auth_index = %q, want stable index %q", accounts[0].AuthIndex, wantAuthIndex)
	}
	for _, secret := range []string{
		"routing-status-credential-id",
		"routing-status-access-token-secret",
		"routing-status-refresh-token-secret",
		"routing-status-api-key-secret",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("response contains secret %q: %s", secret, body)
		}
	}

	_, _, repeated := request()
	if got := repeated.Providers["claude"][0].AuthIndex; got != accounts[0].AuthIndex {
		t.Errorf("repeated auth_index = %q, want %q", got, accounts[0].AuthIndex)
	}
}

func TestGetRoutingStatusWithoutManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(&config.Config{}, "", nil)
	router := gin.New()
	router.GET("/routing/status", handler.GetRoutingStatus)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/routing/status", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got, want := strings.TrimSpace(recorder.Body.String()), `{"error":"core auth manager unavailable"}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}
