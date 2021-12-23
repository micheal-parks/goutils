setup-cert:
	cd etc && bash ./setup_cert.sh

setup-priv-key:
	cd etc && bash ./setup_priv_key.sh

build: buf build-web build-go

build-go:
	go build ./...

build-web:
	cd rpc/js && npm install && npm link && npx webpack
	cd rpc/examples/echo/frontend && npm install && npm link @viamrobotics/rpc && npx webpack
	cd rpc/examples/fileupload/frontend && npm install && npm link @viamrobotics/rpc && npx webpack

buf:
	buf lint
	buf generate
	buf generate --template ./etc/buf.web.gen.yaml buf.build/googleapis/googleapis

lint:
	go install google.golang.org/protobuf/cmd/protoc-gen-go \
      google.golang.org/grpc/cmd/protoc-gen-go-grpc \
      github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway \
      github.com/pseudomuto/protoc-gen-doc/cmd/protoc-gen-doc
	buf lint
	go install github.com/edaniels/golinters/cmd/combined
	go install github.com/golangci/golangci-lint/cmd/golangci-lint
	go list -f '{{.Dir}}' ./... | grep -v gen | grep -v proto | xargs go vet -vettool=`go env GOPATH`/bin/combined
	go list -f '{{.Dir}}' ./... | grep -v gen | grep -v proto | xargs go run github.com/golangci/golangci-lint/cmd/golangci-lint run -v --fix --config=./etc/.golangci.yaml

cover:
	go test -tags=no_skip -race -coverprofile=coverage.txt ./...

test:
	go test -tags=no_skip -race ./...
