package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		token, err := bearerToken(r.Header.Get("Authorization"))
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}

		agentRecord, err := a.db.GetAgentByAPIKeyHash(r.Context(), HashAPIKey(token))
		if err != nil {
			switch {
			case errors.Is(err, db.ErrNotFound):
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
			default:
				writeJSONError(w, http.StatusInternalServerError, "internal authentication failure")
			}
			return
		}
		if agentRecord.DisabledAt != nil {
			writeJSONError(w, http.StatusUnauthorized, "disabled agent key")
			return
		}

		ctx := context.WithValue(r.Context(), contextKey{}, agentRecord)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Authenticator) AdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := bearerToken(r.Header.Get("Authorization"))
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if token != a.adminKey {
			writeJSONError(w, http.StatusUnauthorized, "invalid admin key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", errors.New("missing bearer token")
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("malformed bearer token")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", errors.New("malformed bearer token")
	}
	return token, nil
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
