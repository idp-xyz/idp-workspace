# idp-workspace build and test entry points.
PG_IMAGE ?= public.ecr.aws/docker/library/postgres:17-alpine
PG_NAME  ?= idp-workspace-test-pg
# the object store for file contents (ADR-0001 D24): RustFS, pinned by digest (the platform's kind backups use the same build)
S3_IMAGE  ?= docker.io/rustfs/rustfs:1.0.4-glibc
S3_DIGEST ?= sha256:a9b705b9256be756802a60f3498abdc543918b5b5a4437a57a541e89e733c74d
S3_NAME   ?= idp-workspace-test-s3

.PHONY: build test vet test-integration test-bridge test-web pg-up pg-down s3-up s3-down

build:
	go build -o bin/ ./cmd/...

vet:
	gofmt -l . | (! grep .) && go vet ./...

test: vet
	go test -race ./...

# The app bridge SDK (TypeScript, no dependencies): Node's built-in test runner with type stripping
test-bridge:
	cd packages/bridge && node --test --experimental-strip-types 'test/*.test.ts'

# The frontend (npm workspaces): type check and tests of the bridge SDK and the web shell
test-web:
	npm ci --ignore-scripts --no-audit --no-fund
	npm run typecheck
	npm test

test-integration: pg-up s3-up
	@PORT=$$(docker port $(PG_NAME) 5432/tcp | head -1 | sed 's/.*://'); \
	S3PORT=$$(docker port $(S3_NAME) 9000/tcp | head -1 | sed 's/.*://'); \
	IDP_TEST_PG_OWNER_URL="postgres://postgres:test@127.0.0.1:$$PORT/postgres?sslmode=disable" \
	IDP_TEST_S3_ENDPOINT="http://127.0.0.1:$$S3PORT" IDP_TEST_S3_BUCKET=files IDP_TEST_S3_ACCESS_KEY=itaccess IDP_TEST_S3_SECRET_KEY=it-secret-key-of-the-test-store \
	go test -count=1 -race -v -timeout 10m ./tests/integration/ ; status=$$?; \
	$(MAKE) --no-print-directory pg-down s3-down; exit $$status

pg-up:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 || true
	@docker run -d --name $(PG_NAME) -e POSTGRES_PASSWORD=test -p 127.0.0.1::5432 $(PG_IMAGE) >/dev/null
	@for i in $$(seq 1 60); do docker exec $(PG_NAME) pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break; sleep 0.5; done

pg-down:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 || true

s3-up:
	@docker rm -f $(S3_NAME) >/dev/null 2>&1 || true
	@docker image inspect $(S3_IMAGE) >/dev/null 2>&1 || { docker pull -q $(firstword $(subst :, ,$(S3_IMAGE)))@$(S3_DIGEST) >/dev/null && docker tag $(firstword $(subst :, ,$(S3_IMAGE)))@$(S3_DIGEST) $(S3_IMAGE); }
	@docker run -d --name $(S3_NAME) -p 127.0.0.1::9000 -e RUSTFS_ACCESS_KEY=itaccess -e RUSTFS_SECRET_KEY=it-secret-key-of-the-test-store \
		-e RUSTFS_REGION=us-east-1 -e RUSTFS_CONSOLE_ENABLE=false -e RUSTFS_SSE_S3_MASTER_KEY=$$(openssl rand -base64 32) $(S3_IMAGE) >/dev/null
	@P=$$(docker port $(S3_NAME) 9000/tcp | head -1 | sed 's/.*://'); for i in $$(seq 1 60); do \
		curl -sf --noproxy '*' --aws-sigv4 aws:amz:us-east-1:s3 -u itaccess:it-secret-key-of-the-test-store -X PUT -o /dev/null http://127.0.0.1:$$P/files 2>/dev/null; \
		curl -sf --noproxy '*' --aws-sigv4 aws:amz:us-east-1:s3 -u itaccess:it-secret-key-of-the-test-store -o /dev/null "http://127.0.0.1:$$P/files?list-type=2&max-keys=1" && break; sleep 0.5; done

s3-down:
	@docker rm -f $(S3_NAME) >/dev/null 2>&1 || true
