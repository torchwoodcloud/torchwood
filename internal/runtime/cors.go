package runtime

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

func CORSMiddleware(cfg *config.Http_Cors, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	allowed := cfg.GetAllowOrigins()
	credentials := cfg.GetAllowCredentials()
	if credentials {
		filtered := make([]string, 0, len(allowed))
		for _, o := range allowed {
			if o == "*" {
				logger.Warn("cors: allow_credentials=true with wildcard origin is invalid per CORS spec; ignoring origin", "origin", o)
				continue
			}
			filtered = append(filtered, o)
		}
		allowed = filtered
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			originAllowed := isOriginAllowed(allowed, origin)
			if originAllowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				// P3-5：反射 origin 时必设 Vary: Origin，避免缓存污染。
				w.Header().Set("Vary", "Origin")
			} else if origin != "" {
				// 即使 origin 不匹配也声明 Vary，确保中间缓存按 Origin 区分。
				w.Header().Set("Vary", "Origin")
			}
			if len(cfg.GetAllowMethods()) > 0 {
				w.Header().Set("Access-Control-Allow-Methods", strings.Join(cfg.GetAllowMethods(), ", "))
			}
			if len(cfg.GetAllowHeaders()) > 0 {
				w.Header().Set("Access-Control-Allow-Headers", strings.Join(cfg.GetAllowHeaders(), ", "))
			}
			// P3-5：Allow-Credentials 仅随匹配 origin 输出，避免未匹配时误授权。
			if credentials && originAllowed {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			if cfg.GetMaxAge() > 0 {
				w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int(cfg.GetMaxAge())))
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isOriginAllowed(allowed []string, origin string) bool {
	for _, o := range allowed {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}
