package auth

import (
	"context"
	"fmt"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/envcfg"
	"net/http"
	"os"
	"strings"
	"time"
)

// ResolveInput is how the CLI picks a Bearer for the control-plane.
type ResolveInput struct {
	ExplicitToken string // --id-token / ASP_ID_TOKEN already set by caller
	APIKey        string // ASP_API_KEY fallback (lab without IdP)
	SecretsPath   string // empty → DefaultSecretsPath()
	CachePath     string // empty → DefaultCachePath()
	GrantType     string
	ForceFetch    bool // ignore cache; always hit IdP when credentials exist
	Required      bool // empty → ASP_REQUIRE_TOKEN truthy
	HTTPClient    *http.Client
	Now           func() time.Time // tests; nil → time.Now
}

// ResolveResult is the Bearer to send and how it was obtained.
type ResolveResult struct {
	Bearer string
	Source string // id-token | cache | fetch | api-key | none
	Token  Token  // set when Source is cache or fetch
}

// EnvRequireToken makes the CLI fail when no token can be had, instead of calling the
// control plane without one. It was ASP_IDP_REQUIRED, a name the control plane uses
// for something else (it requires a valid token on every user route) and the node-agent
// for a third (the multi-user profile); the old name is still read, with a warning.
const EnvRequireToken = "ASP_REQUIRE_TOKEN"

// TokenRequired reports whether ASP_REQUIRE_TOKEN (or the old ASP_IDP_REQUIRED) is true.
func TokenRequired() bool {
	v, _, ok := envcfg.Get(nil, EnvRequireToken, "ASP_IDP_REQUIRED")
	if !ok {
		return false
	}
	b, _ := envcfg.ParseBool(v)
	return b
}

// EnvIDToken returns ASP_ID_TOKEN or ASP_IDP_ACCESS_TOKEN.
func EnvIDToken() string {
	if t := strings.TrimSpace(os.Getenv("ASP_ID_TOKEN")); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv("ASP_IDP_ACCESS_TOKEN"))
}

// ResolveBearer picks Authorization Bearer for CP calls.
//
// Priority:
//  1. ExplicitToken / env id-token
//  2. Valid cache
//  3. Fetch from IdP when credentials or ASP_IDP_TOKEN_URL / secrets file available
//  4. API key
//  5. empty (unless Required → error)
func ResolveBearer(ctx context.Context, in ResolveInput) (ResolveResult, error) {
	required := in.Required || TokenRequired()
	explicit := strings.TrimSpace(in.ExplicitToken)
	if explicit == "" {
		explicit = EnvIDToken()
	}
	if explicit != "" {
		return ResolveResult{Bearer: explicit, Source: "id-token"}, nil
	}

	cachePath := in.CachePath
	if cachePath == "" {
		cachePath = DefaultCachePath()
	}
	skew := DefaultSkew()
	if !in.ForceFetch && cachePath != "" {
		if tok, err := LoadCache(cachePath); err == nil && tok.Valid(skew) {
			return ResolveResult{Bearer: tok.AccessToken, Source: "cache", Token: tok}, nil
		}
	}

	secs, canFetch, ferr := loadFetchSecrets(in.SecretsPath)
	if canFetch {
		tok, err := FetchAccessToken(ctx, FetchOptions{
			Secrets:    secs,
			GrantType:  in.GrantType,
			HTTPClient: in.HTTPClient,
		})
		if err != nil {
			if required {
				return ResolveResult{}, fmt.Errorf("idp token fetch: %w", err)
			}
			// fall through to API key
		} else {
			_ = SaveCache(cachePath, tok)
			return ResolveResult{Bearer: tok.AccessToken, Source: "fetch", Token: tok}, nil
		}
		_ = ferr // unused when fetch attempted
	}

	if key := strings.TrimSpace(in.APIKey); key != "" {
		if required {
			// API key is not an IdP JWT; still allow as last resort for service principals,
			// but warn via Source so callers can log.
			return ResolveResult{Bearer: key, Source: "api-key"}, nil
		}
		return ResolveResult{Bearer: key, Source: "api-key"}, nil
	}

	if required {
		msg := EnvRequireToken + " is set but there is no id-token, cache or credentials"
		if ferr != nil {
			msg += ": " + ferr.Error()
		}
		return ResolveResult{}, fmt.Errorf("%s (set ASP_ID_TOKEN, run asp auth login, or provide ASP_IDP_SECRETS_FILE)", msg)
	}
	return ResolveResult{Source: "none"}, nil
}

func loadFetchSecrets(secretsPath string) (Secrets, bool, error) {
	env := SecretsFromEnv()
	path := secretsPath
	if path == "" {
		path = DefaultSecretsPath()
	}
	fileSecs := Secrets{}
	var fileErr error
	if path != "" {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			fileSecs, fileErr = LoadSecretsFile(path)
			if fileErr != nil {
				return Secrets{}, false, fileErr
			}
		} else if env.TokenURL == "" && env.Issuer == "" && env.ClientID == "" {
			// no file and no env credentials
			if path != "" && os.IsNotExist(err) {
				fileErr = fmt.Errorf("secrets file not found: %s", path)
			}
		}
	}
	merged := fileSecs.Merge(env)
	// Enough to attempt fetch?
	if merged.ClientID == "" {
		return merged, false, fileErr
	}
	if _, err := merged.ResolveTokenURL(); err != nil {
		return merged, false, err
	}
	return merged, true, nil
}

// CanAutoFetch reports whether env/secrets look sufficient to hit the token endpoint.
func CanAutoFetch() bool {
	_, ok, _ := loadFetchSecrets("")
	return ok
}
