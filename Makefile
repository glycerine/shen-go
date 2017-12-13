.PHONY: all kl shen

all: kl shen

kl:
	go install github.com/tiancaiamao/shen-go/cmd/kl

shen:
	go install github.com/tiancaiamao/shen-go/cmd/shen

test:
	go test github.com/tiancaiamao/shen-go/vm
	go test github.com/tiancaiamao/shen-go/kl