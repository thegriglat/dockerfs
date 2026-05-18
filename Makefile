BINARY := dockerfs
CMD    := ./cmd/dockerfs

.PHONY: all build clean

all: build

build:
	go build -o $(BINARY) $(CMD)

clean:
	rm -f $(BINARY)
