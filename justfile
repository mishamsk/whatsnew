set dotenv-load := false

setup:
	mise install
	mise exec -- go mod tidy

build:
	mise exec -- go build -o bin/whatsnew .

run:
	mise exec -- go run .

fmt:
	mise exec -- gofmt -w .
