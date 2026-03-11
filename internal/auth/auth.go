package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"gitthicket/internal/db"
	"gitthicket/internal/model"
)

type contextKey struct{}

type Authenticator struct {
	db       *db.DB
	adminKey string
}

func NewAuthenticator(database *db.DB, adminKey string) *Authenticator {
	return &Authenticator{
		db:       database,
		adminKey: adminKey,
	}
}

func GenerateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gtk_" + hex.EncodeToString(buf), nil
}

func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func AgentFromContext(ctx context.Context) (*model.Agent, bool) {
	agent, ok := ctx.Value(contextKey{}).(*model.Agent)
	return agent, ok
}

func (a *Authenticator) AgentMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}

		agentRecord, err := a.db.GetAgentByAPIKeyHash(r.Context(), HashAPIKey(token))
		if err != nil {
			switch {
			case errors.Is(err, db.ErrNotFound):
				http.Error(w, "invalid api key", http.StatusUnauthorized)
			default:
				http.Error(w, "authentication failed", http.StatusInternalServerError)
			}
			return
		}
		if agentRecord.DisabledAt != nil {
			http.Error(w, "invalid api key", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), contextKey{}, agentRecord)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Authenticator) AdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || token != a.adminKey {
			http.Error(w, "invalid admin key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}
