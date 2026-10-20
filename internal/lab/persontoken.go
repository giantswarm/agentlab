package lab

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// personToken is the person's bearer for a command that outlives one token
// (workspaces-measure's runs): the access token muster takes and the ID token
// kagent takes, renewed from their source shortly before they expire, so no
// call goes out with an expired one, the cleanup's included.
type personToken struct {
	// fetch answers the person's current tokens: a fresh password grant on
	// the lab (the Dex id_token for both), the person's muster sign-in on
	// another cluster.
	fetch func() (access, id string, err error)
	now   func() time.Time
	sleep func(time.Duration)

	mu         sync.Mutex
	access, id string
	expiry     time.Time
}

const (
	// personTokenSkew is how close to its expiry a token is renewed: a call
	// never starts with less of it left.
	personTokenSkew = 10 * time.Second
	// personTokenOpaque is how long a token without an exp claim is kept
	// before it is asked for again.
	personTokenOpaque = time.Minute
	// personTokenFetchAttempts bounds how often a failing source is asked
	// before its failure is the call's: a sign-in that fails once (the
	// muster CLI exiting on a transient error) is asked again, pausing from
	// personTokenFetchBackoff, doubling.
	personTokenFetchAttempts = 4
	personTokenFetchBackoff  = 2 * time.Second
)

func newPersonToken(fetch func() (access, id string, err error)) *personToken {
	return &personToken{fetch: fetch, now: time.Now, sleep: time.Sleep}
}

// musterPersonToken is the person's muster sign-in at url: muster renews its
// access token, and with it the ID token, once the access token has expired.
func musterPersonToken(url string) *personToken {
	return newPersonToken(func() (string, string, error) {
		access, err := musterCLIToken(url, false)
		if err != nil {
			return "", "", err
		}
		id, err := musterCLIToken(url, true)
		return access, id, err
	})
}

func (p *personToken) accessToken() (string, error) {
	access, _, err := p.tokens()
	return access, err
}

func (p *personToken) idToken() (string, error) {
	_, id, err := p.tokens()
	return id, err
}

// tokens answers the current pair, renewed when it is about to expire. A
// source that renews only past the expiry (muster's sign-in) answers the
// same pair until then: the renewal waits for the expiry and asks again.
func (p *personToken) tokens() (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.id != "" && p.now().Before(p.expiry.Add(-personTokenSkew)) {
		return p.access, p.id, nil
	}
	renewal := p.id != ""
	for attempt := 0; ; attempt++ {
		access, id, err := p.fetchRetrying()
		if err != nil {
			return "", "", err
		}
		expiry := tokenExpiry(p.now(), access, id)
		if p.now().Before(expiry.Add(-personTokenSkew)) {
			p.access, p.id, p.expiry = access, id, expiry
			if renewal {
				note("the person's token renewed, valid until %s", expiry.Local().Format(time.TimeOnly))
			}
			return access, id, nil
		}
		if attempt > 0 {
			return "", "", fmt.Errorf("the person's token expires at %s and its sign-in did not renew it: sign in again", expiry.Local().Format(time.TimeOnly))
		}
		wait := max(expiry.Sub(p.now())+time.Second, 0)
		note("the person's token expires at %s, its sign-in renews it then: waiting %s", expiry.Local().Format(time.TimeOnly), wait.Round(time.Second))
		p.sleep(wait)
	}
}

// fetchRetrying asks the source up to personTokenFetchAttempts times with a
// growing pause; a source that keeps failing answers its last error, with
// the attempts named.
func (p *personToken) fetchRetrying() (string, string, error) {
	pause := personTokenFetchBackoff
	for attempt := 1; ; attempt++ {
		access, id, err := p.fetch()
		if err == nil {
			return access, id, nil
		}
		if attempt == personTokenFetchAttempts {
			return "", "", fmt.Errorf("the person's token: %d attempts failed, last: %w", attempt, err)
		}
		note("the person's token: attempt %d failed (%v), asking again in %s", attempt, err, pause)
		p.sleep(pause)
		pause *= 2
	}
}

// tokenExpiry is the earliest exp claim of the tokens; tokens without one
// are kept for personTokenOpaque from now.
func tokenExpiry(now time.Time, tokens ...string) time.Time {
	var expiry time.Time
	for _, token := range tokens {
		claims, err := decodeJWTClaims(token)
		if err != nil {
			continue
		}
		if exp, ok := claims[claimExpiry].(float64); ok && exp > 0 {
			if t := time.Unix(int64(exp), 0); expiry.IsZero() || t.Before(expiry) {
				expiry = t
			}
		}
	}
	if expiry.IsZero() {
		return now.Add(personTokenOpaque)
	}
	return expiry
}

// renewingBearer attaches the bearer token answers to every call on a gRPC
// connection, the one current when the call starts, in place of a bearer
// fixed at dial; a token that cannot be renewed fails the call.
func renewingBearer(token func() (string, error)) []grpc.DialOption {
	attach := func(ctx context.Context) (context.Context, error) {
		t, err := token()
		if err != nil {
			return nil, err
		}
		md, _ := metadata.FromOutgoingContext(ctx)
		md = md.Copy()
		md.Set(authorizationMetadata, "Bearer "+t)
		return metadata.NewOutgoingContext(ctx, md), nil
	}
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			ctx, err := attach(ctx)
			if err != nil {
				return err
			}
			return invoker(ctx, method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			ctx, err := attach(ctx)
			if err != nil {
				return nil, err
			}
			return streamer(ctx, desc, cc, method, opts...)
		}),
	}
}
