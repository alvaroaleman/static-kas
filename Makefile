prefix = /usr/local

all: static-kas

# Let Go handle checking if recompilation is needed for ./static-kas
.PHONY: static-kas

static-kas:
	go build -o static-kas ./cmd

clean:
	rm static-kas

install: static-kas
	install -D static-kas $(DESTDIR)/$(prefix)/bin/static-kas
