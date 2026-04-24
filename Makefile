BIN := bin/voice-agent

.PHONY: build clean lint module test

build:
	GOOS=linux GOARCH=amd64 go build -o $(BIN) .

build-local:
	go build -o $(BIN) .

clean:
	rm -rf bin/

lint:
	go vet ./...

test:
	go test ./...

module: build
	tar -czf module.tar.gz $(BIN) meta.json
