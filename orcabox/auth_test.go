package orcabox

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpiredEntitlementReportsBillingInsteadOfInvalidCredentials(t *testing.T) {
	l := testLedger(t)
	now := time.Now().UTC()
	l.Now = func() time.Time { return now }
	_, err := l.Grant("expiry-http-order", "buyer", 100, 30)
	require.NoError(t, err)
	key, err := l.Key("buyer")
	require.NoError(t, err)
	now = now.Add(31 * 24 * time.Hour)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/v1/models", nil)
	c.Request.Header.Set("Authorization", "Bearer "+key)
	gateway(c, l, PriceBook{})
	assert.Equal(t, 402, w.Code)

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/v1/models", nil)
	c.Request.Header.Set("Authorization", "Bearer sk-invalid")
	gateway(c, l, PriceBook{})
	assert.Equal(t, 401, w.Code)
}

func authContext(key string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/usage", nil)
	c.Request.Header.Set("Authorization", "Bearer "+key)
	return c
}

func TestRotationRejectsPreviouslyAuthenticatedAdmission(t *testing.T) {
	l := testLedger(t)
	l.Grant("rotation-order", "buyer", 100, 30)
	oldKey, _ := l.Key("buyer")
	snapshot, e := authenticate(authContext(oldKey), l)
	if e != nil {
		t.Fatal(e)
	}
	if e = l.Rotate("buyer"); e != nil {
		t.Fatal(e)
	}
	if _, e = l.ReserveAuthenticated("buyer", "m", "v1", Unit, snapshot.KeyGeneration); !errors.Is(e, ErrKeyChanged) {
		t.Fatal("stale authenticated request admitted after rotation", e)
	}
	if _, e = authenticate(authContext(oldKey), l); !errors.Is(e, ErrKeyChanged) {
		t.Fatal("old key still authenticates", e)
	}
	newKey, _ := l.Key("buyer")
	current, e := authenticate(authContext(newKey), l)
	if e != nil || current.KeyGeneration != snapshot.KeyGeneration+1 {
		t.Fatal("rotation generation not atomic", e)
	}
	if current.Available != 100*Unit || current.Frozen != 0 {
		t.Fatal("stale credential consumed funds")
	}
	if _, e = l.ReserveAuthenticated("buyer", "m", "v1", Unit, current.KeyGeneration); e != nil {
		t.Fatal(e)
	}
}

func TestAuthenticationRejectsDeletedTokenAndPreservesOutageStatus(t *testing.T) {
	l := testLedger(t)
	a, _ := l.Grant("auth-order", "buyer", 100, 30)
	key, _ := l.Key("buyer")
	if e := l.DB.Delete(&model.Token{}, a.TokenID).Error; e != nil {
		t.Fatal(e)
	}
	if _, e := authenticate(authContext(key), l); !errors.Is(e, ErrKeyChanged) {
		t.Fatal("soft-deleted key authenticated", e)
	}
	sql, _ := l.DB.DB()
	sql.Close()
	if _, e := authenticate(authContext(key), l); !errors.Is(e, ErrAuthUnavailable) {
		t.Fatal("database outage reported as invalid key", e)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = authContext(key).Request
	gateway(c, l, PriceBook{})
	if w.Code != 503 {
		t.Fatal("client would discard valid credentials during outage", w.Code)
	}
}
