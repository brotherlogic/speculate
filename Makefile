.PHONY: all proto test build clean lint

all: test build

proto:
	@if [ -d "proto" ] && ls proto/*/*.proto >/dev/null 2>&1 || [ -d "example/proto" ] && ls example/proto/*/*.proto >/dev/null 2>&1; then \
		for f in $$(find proto example/proto -name "*.proto" 2>/dev/null); do \
			echo "Compiling $$f..."; \
			protoc --go_out=. --go_opt=paths=source_relative \
				--go-grpc_out=. --go-grpc_opt=paths=source_relative \
				"$$f"; \
		done \
	else \
		echo "No proto files found to compile"; \
	fi

test:
	go test -v ./...

build:
	go build ./...

clean:
	go clean ./...

lint:
	go vet ./...
