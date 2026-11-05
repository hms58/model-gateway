.PHONY: build run test lint fmt clean

GOOS=$(shell go env GOOS)

srvName=model-gateway

ifeq ($(GOOS),windows)
	appName=$(srvName).exe
else
	appName=$(srvName)
endif

build:
	go build -o $(appName) ./cmd/$(srvName)

run: build
	./$(appName) -c ./configs/modelgw.example.json

# -gcflags=all=-l 关闭内联，保证依赖 monkey 打桩的用例生效；只作用于测试编译。
test:
	go test -gcflags=all=-l ./... -cover

lint:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(appName)
