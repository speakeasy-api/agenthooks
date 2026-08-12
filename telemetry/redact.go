package telemetry

import (
	"net/url"
	"regexp"
	"strings"
)

// Built-in transport-credential redaction, ported from the proven relay
// implementation (gram/hooks/relay/redact.go). Credential material can ride
// along in MCP server transport — basic-auth userinfo and secret-named query
// parameters in a URL, or secret flags/tokens in a stdio launch command —
// and in captured content. Everything is redacted before touching the disk
// spool; host, path, and non-secret arguments survive so transport identity
// stays matchable server-side. The consumer's Redactor runs after this.

var (
	secretParamRE    = regexp.MustCompile(`(?i)(key|token|secret|password|passwd|credential|auth)`)
	signatureParamRE = regexp.MustCompile(`(?i)^(sig|signature|x-amz-signature|x-goog-signature)$`)
)

// redactURL strips basic-auth userinfo and fragments and masks secret-named
// query values while preserving the host, path, and benign parameters. An
// unparseable URL could hide credentials anywhere, so it is dropped entirely
// (matchable identity is already hopeless for it) — the one divergence from
// the relay port, which returns such input untouched.
func redactURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "***"
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			if secretParamRE.MatchString(k) || signatureParamRE.MatchString(k) {
				q.Set(k, "***")
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

var (
	secretEnvAssignRE = regexp.MustCompile(`(?i)^[A-Za-z_][A-Za-z0-9_]*(key|token|secret|password|passwd|credential|auth)[A-Za-z0-9_]*=`)
	secretAssignRE    = regexp.MustCompile(`(?i)^--?[^=]*(key|token|secret|password|passwd|credential|bearer|auth)[^=]*=`)
	secretFlagRE      = regexp.MustCompile(`(?i)^--?[^=]*(key|token|secret|password|passwd|credential|bearer|auth)[^=]*$`)
	// The optional prefix covers --header=NAME:..., and curl's attached
	// short-option form (-HNAME:... after quote stripping). Alongside the
	// known names, any header name carrying a secret keyword counts
	// (api-key, X-Auth-Token) — guarded at the call site against URL tokens,
	// whose scheme can carry a keyword too (oauth://).
	secretHeaderRE        = regexp.MustCompile(`(?i)^(--?[^=]*=|-[a-z])?(authorization|proxy-authorization|cookie|x-api-key) *:`)
	genericSecretHeaderRE = regexp.MustCompile(`(?i)^(--?[^=]*=|-[a-z])?[a-z0-9-]*(key|token|secret|password|passwd|credential|auth)[a-z0-9-]* *:`)
	envAssignRE           = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// authSchemeRE matches auth scheme words that precede the credential in a
	// header value ("Authorization: Token abc"); the scheme is not the secret.
	authSchemeRE  = regexp.MustCompile(`(?i)^(bearer|basic|token|digest|negotiate|ntlm|dpop|oauth|hawk|apikey)$`)
	tokenPrefixRE = regexp.MustCompile(`(?i)://[^/@]*@|^(sk-|ghp_|gho_|github_pat_|xox[a-z]-|glpat-)`)
)

// redactCommand masks secret flag values and inline tokens in a stdio MCP
// launch command. Tokenization splits on spaces and cannot see through shell
// quoting; the patterns cover the common unquoted shapes, so a repointed
// server keeps a stable redacted identity.
func redactCommand(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.ReplaceAll(raw, `"`, "")
	raw = strings.ReplaceAll(raw, "'", "")
	fields := strings.Fields(raw)
	out := make([]string, 0, len(fields))
	maskNext := false
	// schemeNext marks a pending mask that came from a header, whose value may
	// open with an auth-scheme word ("authorization: bearer TOKEN"); the
	// scheme is not the secret, so the mask rides through to the credential.
	// A secret flag's value gets no such exception — it is the secret even
	// when it collides with a scheme word.
	schemeNext := false
	// cookieNext continues a masked multi-part cookie ("Cookie: sid=abc;
	// csrf=def") whose fragments tokenize separately, but only through
	// cookie-pair-shaped tokens, so a trailing ';' on the last fragment
	// cannot swallow an unrelated following argument. cookiePending scopes
	// the continuation to cookie headers: other secret values ending in ';'
	// do not fragment.
	cookieNext := false
	cookiePending := false
	for _, f := range fields {
		if maskNext {
			if schemeNext && authSchemeRE.MatchString(f) {
				out = append(out, f)
				schemeNext = false
				continue
			}
			out = append(out, "***")
			cookieNext = cookiePending && strings.HasSuffix(f, ";")
			maskNext, schemeNext, cookiePending = false, false, false
			continue
		}
		if cookieNext {
			cookieNext = false
			if strings.Contains(f, "=") && !strings.Contains(f, "://") {
				out = append(out, "***")
				cookieNext = strings.HasSuffix(f, ";")
				continue
			}
			// Not a cookie pair: the value ended at the ';'.
		}
		switch {
		case secretEnvAssignRE.MatchString(f):
			i := strings.IndexByte(f, '=')
			out = append(out, f[:i+1]+"***")
		case secretAssignRE.MatchString(f):
			if i := strings.IndexByte(f, '='); i >= 0 {
				out = append(out, f[:i+1]+"***")
			} else {
				out = append(out, "***")
			}
		case secretFlagRE.MatchString(f):
			out = append(out, f)
			maskNext = true
		// A token is URL-shaped only when its first colon opens "://"; a
		// keyword-bearing scheme (oauth://) must not read as a header, and a
		// no-space header whose value is a URL must not read as one.
		case secretHeaderRE.MatchString(f) || (strings.Index(f, "://") != strings.IndexByte(f, ':') && genericSecretHeaderRE.MatchString(f)):
			// `--header "X-API-Key: abc"` tokenizes the value into the next
			// field after quote stripping; a header with nothing after its
			// colon — or with only an auth scheme, as in
			// "Authorization:Bearer TOKEN" — must keep the mask pending for
			// the credential that follows.
			i := strings.IndexByte(f, ':')
			value := strings.TrimSpace(f[i+1:])
			isCookie := strings.Contains(strings.ToLower(f[:i]), "cookie")
			switch {
			case value == "":
				out = append(out, f[:i]+":")
				maskNext, schemeNext = true, true
				cookiePending = isCookie
			case authSchemeRE.MatchString(value):
				out = append(out, f[:i]+": "+value)
				maskNext = true
			default:
				out = append(out, f[:i]+": ***")
				// A no-space header ("Cookie:sid=abc; csrf=def") carries the
				// first fragment in this token; a ';'-terminated value means
				// more fragments follow.
				cookieNext = isCookie && strings.HasSuffix(value, ";")
			}
		case strings.EqualFold(f, "bearer"):
			out = append(out, f)
			maskNext = true
		case envAssignRE.MatchString(f) && tokenPrefixRE.MatchString(f[strings.IndexByte(f, '=')+1:]):
			// An env assignment whose name has no secret keyword can still
			// carry a recognizable credential ("GITHUB_PAT=github_pat_...",
			// a userinfo URL); the value's shape gives it away.
			i := strings.IndexByte(f, '=')
			out = append(out, f[:i+1]+"***")
		case strings.Contains(f, "://"):
			// A server URL passed as an argument (npx mcp-remote <url>) can
			// carry credentials in userinfo or its query string; structured
			// redaction keeps the host and path matchable. Checked before
			// tokenPrefixRE so userinfo URLs are stripped, not swallowed
			// whole. A token the parser cannot make sense of could hide
			// credentials anywhere, so it is dropped entirely.
			if _, err := url.Parse(f); err != nil {
				out = append(out, "***")
			} else {
				out = append(out, redactURL(f))
			}
		case tokenPrefixRE.MatchString(f):
			out = append(out, "***")
		default:
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// Content redaction is conservative on purpose: prompt text, tool IO, and
// assistant messages are free-form, so only unambiguous credential shapes
// are masked in place, never restructured.
var (
	contentTokenRE    = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|ghp_[A-Za-z0-9]{8,}|gho_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]{8,}|xox[a-z]-[A-Za-z0-9-]{8,}|glpat-[A-Za-z0-9_-]{8,})`)
	contentBearerRE   = regexp.MustCompile(`(?i)\b(bearer|authorization: *(?:bearer|basic|token)?) +[A-Za-z0-9._~+/=-]{8,}`)
	contentEnvRE      = regexp.MustCompile(`(?i)\b([A-Za-z_][A-Za-z0-9_]*(?:key|token|secret|password|passwd|credential|auth)[A-Za-z0-9_]*=)[^\s"']+`)
	contentUserinfoRE = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/\s@]+@`)
)

// redactContent masks unambiguous credential material inside free-form
// captured content (token-prefixed secrets, bearer credentials, secret-named
// env assignments, URL userinfo) while leaving the surrounding text intact.
func redactContent(s string) string {
	if s == "" {
		return ""
	}
	s = contentTokenRE.ReplaceAllString(s, "***")
	s = contentBearerRE.ReplaceAllString(s, "$1 ***")
	s = contentEnvRE.ReplaceAllString(s, "$1***")
	s = contentUserinfoRE.ReplaceAllString(s, "$1***@")
	return s
}
