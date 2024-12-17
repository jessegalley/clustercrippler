export CGO_ENABLED=0

build:
	go build -o bin/clustercrippler clustercrippler.go

run: build
	./bin/clustercrippler

clean:
	rm -f bin/clustercrippler

test:
	go test -v ./... 
