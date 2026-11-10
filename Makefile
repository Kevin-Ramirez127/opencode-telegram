.PHONY: build run test vet fmt chmod-scripts install uninstall start stop restart status logs reload check config settings simulate

BIN := bin/opencode-telegram

install:
	./install.sh

uninstall:
	rm -f $(HOME)/.local/bin/opencode-telegram
	rm -f $(HOME)/.local/share/opencode-telegram/scripts/daemon.sh
	rm -f $(HOME)/.local/share/opencode-telegram/scripts/simulate.sh
	rm -f $(HOME)/.local/share/opencode-telegram/bin/opencode-telegram
	@echo "kept user state in ~/.local/share/opencode-telegram (.env, data/, logs)"

start:
	./scripts/daemon.sh start

stop:
	./scripts/daemon.sh stop

restart:
	./scripts/daemon.sh restart

status:
	./scripts/daemon.sh status

logs:
	./scripts/daemon.sh logs

reload:
	./scripts/daemon.sh reload

check:
	./scripts/daemon.sh check

config:
	./scripts/daemon.sh config

settings:
	./scripts/daemon.sh settings

simulate:
	./scripts/simulate.sh $(SCENARIO)

build:
	go build -ldflags "-X main.version=$$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o $(BIN) .

run: build
	./$(BIN)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

chmod-scripts:
	chmod +x scripts/*.sh install.sh
