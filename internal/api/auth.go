package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type ctxKey int

const (
	userIDKey ctxKey = iota
	isAdminKey
)

// authenticate resolves identity from the Authorization header and nowhere
// else. Handlers read the user id out of the request context, so a body field
// named user_id has no path into the booking logic at all.
func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || strings.TrimSpace(raw) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing_token"})
			return
		}

		token, err := jwt.Parse(strings.TrimSpace(raw), func(t *jwt.Token) (any, error) {
			// Guards against algorithm confusion: without this a token signed
			// with alg=none, or an RS256 token using our public key as an HMAC
			// secret, would be accepted.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
			}
			return []byte(s.cfg.JWTSecret), nil
		}, jwt.WithValidMethods([]string{"HS256"}))

		if err != nil {
			reason := "invalid_token"
			if strings.Contains(err.Error(), "expired") {
				reason = "expired_token"
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": reason})
			return
		}

		sub, err := token.Claims.GetSubject()
		if err != nil || sub == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
			return
		}

		admin := false
		if claims, ok := token.Claims.(jwt.MapClaims); ok {
			admin, _ = claims["admin"].(bool)
		}

		ctx := context.WithValue(r.Context(), userIDKey, sub)
		ctx = context.WithValue(ctx, isAdminKey, admin)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		if admin, _ := r.Context().Value(isAdminKey).(bool); !admin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin_required"})
			return
		}
		next(w, r)
	})
}

func userID(r *http.Request) string {
	id, _ := r.Context().Value(userIDKey).(string)
	return id
}

// devToken mints a token so the load generator and the verify script can
// authenticate without a user store. Gated behind ENABLE_DEV_TOKEN and not
// registered at all when that flag is off.
func (s *Server) devToken(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user_id")
	if user == "" {
		badRequest(w, "user_id query parameter is required")
		return
	}

	claims := jwt.MapClaims{
		"sub": user,
		"exp": time.Now().Add(24 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	if r.URL.Query().Get("admin") == "1" {
		claims["admin"] = true
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(s.cfg.JWTSecret))
	if err != nil {
		s.log.Error("cannot sign dev token", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sign_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": signed, "user_id": user})
}
