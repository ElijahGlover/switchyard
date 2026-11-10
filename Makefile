BUCKET   ?=
STACK    ?= switchyard
REGION   ?= ap-southeast-2
PREFIX   ?= routing
DATA     ?= examples
RESERVED ?= 0
TEMPLATE ?= deploy/function-url.yaml
PARAMS   ?=
FUZZTIME ?= 30s

STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0

GO_SOURCES := $(shell find cmd internal -name '*.go' -not -name '*_test.go') go.mod go.sum

.PHONY: test lint fuzz cover bench examples check bucket deploy upload clean require-bucket

test:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)
	go vet ./...
	go test -race ./...

lint:
	go mod tidy -diff
	go run $(STATICCHECK) ./...
	go run $(GOVULNCHECK) ./...

fuzz:
	go test -run '^$$' -fuzz FuzzResolveNeverLeavesAllowedSuffixes -fuzztime $(FUZZTIME) ./internal/route/
	go test -run '^$$' -fuzz FuzzExplainMatchesResolve -fuzztime $(FUZZTIME) ./internal/route/
	go test -run '^$$' -fuzz FuzzConfig -fuzztime $(FUZZTIME) ./internal/route/

cover:
	@mkdir -p build
	go test -coverprofile=build/cover.out ./...
	go tool cover -func=build/cover.out | awk '$$3+0 < 100'

bench:
	go test -run '^$$' -bench . -benchmem ./internal/route/ ./cmd/router/

examples: examples/asn.mmdb examples/netmap.mmdb

examples/%.mmdb: examples/%.csv
	go run ./cmd/mkmmdb $< $@

check: examples
	go run ./cmd/validate $(DATA)/config.yaml

build/bootstrap: $(GO_SOURCES)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -ldflags='-s -w' -o $@ ./cmd/router

build/router.zip: build/bootstrap
	cd build && rm -f router.zip && zip -q router.zip bootstrap

# One-off: the bucket holds the routing data under $(PREFIX)/ and the
# function code under code/.
bucket: require-bucket
	aws s3 mb s3://$(BUCKET) --region $(REGION)

# DEBUG_KEY turns on the debug header until a deploy without it. The recipe
# reads it from the shell, so make never echoes the key. PARAMS passes the
# parameters a template adds, such as ListenerArn=... for alb.yaml.
deploy: require-bucket build/router.zip
	aws cloudformation package --region $(REGION) \
		--template-file $(TEMPLATE) \
		--s3-bucket $(BUCKET) --s3-prefix code \
		--output-template-file build/packaged.yaml
	aws cloudformation deploy --region $(REGION) \
		--template-file build/packaged.yaml \
		--stack-name $(STACK) \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides Bucket=$(BUCKET) Prefix=$(PREFIX) ReservedConcurrency=$(RESERVED) DebugKey="$$DEBUG_KEY" $(PARAMS)
	@aws cloudformation describe-stacks --region $(REGION) --stack-name $(STACK) \
		--query 'Stacks[0].Outputs[?OutputKey==`Url`].OutputValue' --output text

# Databases go up before the config, so a new config never names a database
# that is not there yet.
upload: require-bucket check
	aws s3 sync $(DATA)/ s3://$(BUCKET)/$(PREFIX)/ --region $(REGION) --exclude '*' --include '*.mmdb'
	aws s3 cp $(DATA)/config.yaml s3://$(BUCKET)/$(PREFIX)/config.yaml --region $(REGION)

clean:
	rm -rf build examples/*.mmdb

require-bucket:
	@test -n "$(BUCKET)" || (echo "set BUCKET, e.g. make deploy BUCKET=my-switchyard" && exit 1)
