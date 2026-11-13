// Command idp-workspace is the IDP Workspace: workspaces, the app registry and installations, the
// work inbox, work contexts and action invocation (ADR-0001).
//
//	idp-workspace [serve]                          run the API
//	idp-workspace migrate                          apply schema migrations (owner connection)
//	idp-workspace ensure-login USER                create/update a login role in idp_ws_app; password from IDP_APP_DB_PASSWORD[_FILE]
//	idp-workspace seed-tenant SLUG [ID]            create a tenant, print TENANT_ID=<id>
//	idp-workspace dev-token TENANT SUBJECT ROLE..  print a development token (dev mode only)
//
// Environment:
//
//	IDP_DATABASE_URL             service connection (a member of idp_ws_app, never a superuser)
//	IDP_MIGRATION_DATABASE_URL   schema owner connection (migrate, ensure-login, seed-tenant)
//	IDP_WORKSPACE_LISTEN         default 127.0.0.1:8560
//	IDP_WORKSPACE_WEB_DIR        the built web shell (apps/web/dist) served at / (ADR-0001 D9); unset = API only
//	IDP_WORKSPACE_OIDC_CLIENT_ID the shell's public OIDC client (authorization code + PKCE at IDP_WORKSPACE_ISSUER, ADR-0001 D14); IDP_WORKSPACE_OIDC_SCOPE (default "openid")
//	IDP_WORKSPACE_PUBLIC_URL     the workspace's own URL, issuer of app tokens (D15)
//	IDP_WORKSPACE_SIGNING_KEY_FILE   P-256 PEM key signing app tokens; in dev mode an ephemeral key when unset
//	IDP_WORKSPACE_CONFIG         optional YAML: action targets (idp-host, idp-eer) and the webhook secret (ADR-0001 D4)
//	IDP_DEV_MODE                 "true": HS256 development tokens
//	IDP_WORKSPACE_ISSUER / IDP_WORKSPACE_AUDIENCE   accepted tokens (default idp-workspace-dev / idp-workspace)
//	IDP_WORKSPACE_JWKS_URL       production keys; IDP_DEV_SECRET[_FILE] (>= 32 bytes) in dev mode instead
//	IDP_WORKSPACE_TENANT_CLAIM / IDP_WORKSPACE_ROLES_CLAIM / IDP_WORKSPACE_SUBJECT_CLAIM   claims (default tenant_id / roles / sub)
//	IDP_WORKSPACE_TLS_CERT_FILE / IDP_WORKSPACE_TLS_KEY_FILE   serve HTTPS
//	IDP_IDENTITY_CA_FILE         extra trusted CAs for the JWKS endpoint
//	IDP_METRICS_LISTEN           serve Prometheus metrics (/metrics) on this address; off when unset
//	IDP_LOG_LEVEL                debug | info | warn | error
//	IDP_WORKSPACE_INTERNAL_LISTEN  the internal file endpoint for idp-sandbox (ADR-0001 D23), e.g. 0.0.0.0:8561; set (with
//	                             app tokens' IDP_WORKSPACE_PUBLIC_URL), host.run actions get file tickets; reachable only from the sandbox
//	IDP_WORKSPACE_FILE_MB (25) IDP_WORKSPACE_FILE_QUOTA_MB (200) IDP_WORKSPACE_FILE_QUOTA_FILES (500)
//	IDP_WORKSPACE_FILE_RETENTION_DAYS (30) IDP_WORKSPACE_FILE_TICKET_MIN (60) IDP_WORKSPACE_RUN_FILES (10)   members' files
//	IDP_WORKSPACE_FILE_STORE     postgres (default) or s3: where file contents live (ADR-0001 D24); s3 takes
//	                             IDP_WORKSPACE_S3_ENDPOINT (http(s)://host:port), _S3_BUCKET, _S3_REGION (us-east-1),
//	                             _S3_ACCESS_KEY_FILE, _S3_SECRET_KEY_FILE, _S3_CA_FILE (optional), _S3_SSE (default true: SSE-S3)
//	OTEL_EXPORTER_OTLP_ENDPOINT ...   OTLP/HTTP trace export (off without an endpoint)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"idp-workspace/adapters/blob"
	"idp-workspace/adapters/identity"
	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/api"
	"idp-workspace/internal/apptoken"
	"idp-workspace/internal/config"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/files"
	"idp-workspace/internal/observability"
	"idp-workspace/internal/runs"
	"idp-workspace/internal/transport"
)

var version = "0.1.0-dev"

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) (int64, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func secret(key string) string {
	if v := env(key, ""); v != "" {
		return v
	}
	if f := env(key+"_FILE", ""); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func profile() identity.Profile {
	return identity.Profile{Issuer: env("IDP_WORKSPACE_ISSUER", "idp-workspace-dev"), Audience: env("IDP_WORKSPACE_AUDIENCE", "idp-workspace"), JWKSURL: env("IDP_WORKSPACE_JWKS_URL", ""),
		TenantClaim: env("IDP_WORKSPACE_TENANT_CLAIM", "tenant_id"), RolesClaim: env("IDP_WORKSPACE_ROLES_CLAIM", "roles"), SubjectClaim: env("IDP_WORKSPACE_SUBJECT_CLAIM", "sub")}
}

func main() {
	log := observability.NewLogger(os.Stdout, env("IDP_LOG_LEVEL", "info"))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "migrate":
		var applied []string
		applied, err = postgres.Migrate(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""))
		for _, a := range applied {
			fmt.Println("applied", a)
		}
	case "ensure-login":
		if len(os.Args) < 3 {
			err = errors.New("usage: idp-workspace ensure-login USER")
			break
		}
		err = postgres.EnsureLogin(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""), os.Args[2], secret("IDP_APP_DB_PASSWORD"))
	case "seed-tenant":
		if len(os.Args) < 3 {
			err = errors.New("usage: idp-workspace seed-tenant SLUG [ID]")
			break
		}
		id := ""
		if len(os.Args) > 3 {
			id = os.Args[3]
		}
		var out string
		if out, err = postgres.SeedTenant(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""), id, os.Args[2]); err == nil {
			fmt.Println("TENANT_ID=" + out)
		}
	case "dev-token":
		if len(os.Args) < 5 || env("IDP_DEV_MODE", "false") != "true" {
			err = errors.New("usage: idp-workspace dev-token TENANT SUBJECT ROLE... (dev mode only)")
			break
		}
		var tok string
		tok, err = identity.DevToken([]byte(secret("IDP_DEV_SECRET")), profile(), os.Args[3], os.Args[2], os.Args[4:], 12*time.Hour)
		fmt.Println(tok)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error(cmd+" failed", "error", err)
		os.Exit(1)
	}
}

func downstream(d *config.Downstream) (*dispatch.Downstream, error) {
	tlsCfg, err := transport.ClientTLS(d.CAFile, "", "")
	if err != nil {
		return nil, err
	}
	out := &dispatch.Downstream{Endpoint: d.Endpoint, Auth: d.Auth, Timeout: d.Timeout, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg}}}
	switch d.Auth {
	case dispatch.AuthStatic:
		out.Token = d.Token
	case dispatch.AuthExchange: // ADR-0001 D16
		xTLS, err := transport.ClientTLS(d.Exchange.CAFile, "", "")
		if err != nil {
			return nil, err
		}
		out.Exchange = &dispatch.Exchanger{TokenURL: d.Exchange.TokenURL, ClientID: d.Exchange.ClientID, ClientSecret: d.Exchange.Secret, Audience: d.Exchange.Audience,
			HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: xTLS}}}
	}
	return out, nil
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tp, stopTracing, err := observability.Setup("idp-workspace", version, log)
	if err != nil {
		return err
	}
	defer func() {
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stopTracing(fctx)
	}()
	dev := env("IDP_DEV_MODE", "false") == "true"
	identityTLS, err := transport.ClientTLS(env("IDP_IDENTITY_CA_FILE", ""), "", "")
	if err != nil {
		return fmt.Errorf("IDP_IDENTITY_CA_FILE: %w", err)
	}
	prof := profile()
	var verifier *identity.Verifier
	switch {
	case prof.JWKSURL != "":
		verifier = identity.NewVerifier(prof, &identity.JWKS{URL: prof.JWKSURL, HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: identityTLS}}, TTL: 10 * time.Minute, MinRefresh: 30 * time.Second})
	case dev && secret("IDP_DEV_SECRET") != "":
		if verifier, err = identity.NewDevVerifier(prof, []byte(secret("IDP_DEV_SECRET"))); err != nil {
			return err
		}
	default:
		return errors.New("configure IDP_WORKSPACE_JWKS_URL (or, in dev mode, IDP_DEV_SECRET)")
	}
	cfg, err := config.Load(env("IDP_WORKSPACE_CONFIG", ""))
	if err != nil {
		return fmt.Errorf("IDP_WORKSPACE_CONFIG: %w", err)
	}
	d := &dispatch.Dispatcher{}
	if h := cfg.Targets.Host; h != nil {
		if d.Host, err = downstream(h); err != nil {
			return fmt.Errorf("targets.host: %w", err)
		}
	}
	var cmi *dispatch.Downstream
	if c := cfg.Targets.CMI; c != nil { // ADR-0001 D21
		if cmi, err = downstream(c); err != nil {
			return fmt.Errorf("targets.cmi: %w", err)
		}
	}
	if e := cfg.Targets.EER; e != nil {
		if d.EER, err = downstream(&e.Downstream); err != nil {
			return fmt.Errorf("targets.eer: %w", err)
		}
		d.EERTenant = e.TenantID
	}
	if w := cfg.Webhooks; w != nil {
		b, err := os.ReadFile(w.SecretFile)
		if err != nil {
			return err
		}
		d.WebhookSecret = []byte(strings.TrimSpace(string(b)))
		tlsCfg, err := transport.ClientTLS(w.CAFile, "", "")
		if err != nil {
			return fmt.Errorf("webhooks: %w", err)
		}
		timeout := w.Timeout
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		d.WebhookHTTP = &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	store, err := postgres.Open(ctx, env("IDP_DATABASE_URL", ""))
	if err != nil {
		return err
	}
	defer store.Close()
	switch kind := env("IDP_WORKSPACE_FILE_STORE", "postgres"); kind { // ADR-0001 D24
	case "postgres":
	case "s3":
		s3, err := blob.New(blob.Config{Endpoint: env("IDP_WORKSPACE_S3_ENDPOINT", ""), Bucket: env("IDP_WORKSPACE_S3_BUCKET", ""), Region: env("IDP_WORKSPACE_S3_REGION", "us-east-1"),
			AccessKeyFile: env("IDP_WORKSPACE_S3_ACCESS_KEY_FILE", ""), SecretKeyFile: env("IDP_WORKSPACE_S3_SECRET_KEY_FILE", ""), CAFile: env("IDP_WORKSPACE_S3_CA_FILE", ""),
			SSE: env("IDP_WORKSPACE_S3_SSE", "true") != "false"})
		if err != nil {
			return fmt.Errorf("IDP_WORKSPACE_FILE_STORE=s3: %w", err)
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = s3.Check(cctx)
		cancel()
		if err != nil {
			return fmt.Errorf("the file object store does not answer (bucket %s): %w", s3.Bucket, err)
		}
		store.Blobs = s3
		log.Info("file contents in the object store", "endpoint", s3.Endpoint.String(), "bucket", s3.Bucket, "sse", s3.SSE)
	default:
		return fmt.Errorf("IDP_WORKSPACE_FILE_STORE must be postgres or s3, not %q", kind)
	}
	metrics := observability.NewMetrics()
	if ms := observability.ServeMetrics(env("IDP_METRICS_LISTEN", ""), metrics, log); ms != nil {
		defer ms.Close()
	}
	a := &api.API{Store: store, Auth: verifier, Dispatcher: d, CMI: cmi, Log: log, Metrics: metrics, Version: version, DevMode: verifier.Dev(), WebDir: env("IDP_WORKSPACE_WEB_DIR", "")}
	if id := env("IDP_WORKSPACE_OIDC_CLIENT_ID", ""); id != "" { // ADR-0001 D14
		a.OIDC = &api.ShellOIDC{Issuer: prof.Issuer, ClientID: id, Scope: env("IDP_WORKSPACE_OIDC_SCOPE", "openid")}
	}
	if pub := env("IDP_WORKSPACE_PUBLIC_URL", ""); pub != "" { // ADR-0001 D15
		switch keyFile := env("IDP_WORKSPACE_SIGNING_KEY_FILE", ""); {
		case keyFile != "":
			if a.AppTokens, err = apptoken.Load(pub, keyFile); err != nil {
				return fmt.Errorf("IDP_WORKSPACE_SIGNING_KEY_FILE: %w", err)
			}
		case dev:
			if a.AppTokens, err = apptoken.Generate(pub); err != nil {
				return err
			}
			log.Warn("app tokens are signed with an ephemeral key (dev mode); they stop verifying after a restart")
		default:
			return errors.New("IDP_WORKSPACE_PUBLIC_URL needs IDP_WORKSPACE_SIGNING_KEY_FILE")
		}
	}
	// ADR-0001 D23: members' files, and the tickets runs get for them
	var fileMB, quotaMB, quotaFiles, days, ticketMin, runFiles int64
	for _, x := range []struct {
		dst *int64
		key string
		def int64
	}{{&fileMB, "IDP_WORKSPACE_FILE_MB", 25}, {&quotaMB, "IDP_WORKSPACE_FILE_QUOTA_MB", 200}, {&quotaFiles, "IDP_WORKSPACE_FILE_QUOTA_FILES", 500},
		{&days, "IDP_WORKSPACE_FILE_RETENTION_DAYS", 30}, {&ticketMin, "IDP_WORKSPACE_FILE_TICKET_MIN", 60}, {&runFiles, "IDP_WORKSPACE_RUN_FILES", 10}} {
		if *x.dst, err = envInt(x.key, x.def); err != nil {
			return err
		}
	}
	a.Files = api.FileConfig{MaxBytes: fileMB << 20, QuotaBytes: quotaMB << 20, QuotaFiles: int(quotaFiles), Retention: time.Duration(days) * 24 * time.Hour, RunFiles: int(runFiles)}
	var internal *http.Server
	if addr := env("IDP_WORKSPACE_INTERNAL_LISTEN", ""); addr != "" {
		if a.AppTokens == nil {
			return errors.New("IDP_WORKSPACE_INTERNAL_LISTEN needs app tokens (IDP_WORKSPACE_PUBLIC_URL): file tickets are signed with their key")
		}
		a.Files.Tickets = &files.Ticketer{Signer: a.AppTokens, TTL: time.Duration(ticketMin) * time.Minute}
		d.Tickets, d.DeliverFiles, d.DeliverBytes = a.Files.Tickets, int(runFiles), fileMB<<20
		internal = &http.Server{Addr: addr, Handler: a.InternalHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 120 * time.Second, WriteTimeout: 120 * time.Second}
	}
	go func() { // expired files go
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if n, err := store.PurgeExpiredFiles(ctx); err != nil {
				log.Warn("purging expired files failed", "error", err)
			} else if n > 0 {
				log.Info("purged expired files", "n", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	if d.Host != nil { // ADR-0001 D11: results of host.run actions come back into the inbox
		a.Runs = &runs.Follower{Store: store, Endpoint: d.Host.Endpoint, HTTP: d.Host.HTTP, Log: log}
	}
	srv := &http.Server{Addr: env("IDP_WORKSPACE_LISTEN", "127.0.0.1:8560"), Handler: observability.HTTPMiddleware(tp, "idp-workspace/api", a.Handler()),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	if cert := env("IDP_WORKSPACE_TLS_CERT_FILE", ""); cert != "" {
		if srv.TLSConfig, err = transport.ServerTLS(cert, env("IDP_WORKSPACE_TLS_KEY_FILE", "")); err != nil {
			return fmt.Errorf("TLS: %w", err)
		}
	}
	errc := make(chan error, 2)
	if internal != nil {
		go func() { errc <- internal.ListenAndServe() }()
		defer internal.Close()
	}
	go func() {
		if srv.TLSConfig != nil {
			errc <- srv.ListenAndServeTLS("", "")
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	log.Info("idp-workspace listening", "addr", srv.Addr, "devMode", dev, "tls", srv.TLSConfig != nil, "host", d.Host != nil, "eer", d.EER != nil, "cmi", cmi != nil, "webhooks", d.WebhookSecret != nil, "web", a.WebDir != "", "oidc", a.OIDC != nil, "appTokens", a.AppTokens != nil, "fileTickets", internal != nil)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
