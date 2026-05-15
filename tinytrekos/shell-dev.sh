#!/usr/bin/env bash

docker run \
	--rm \
	-it \
	-v $(pwd):$(pwd) \
	-v $SSH_AUTH_SOCK:/ssh-agent \
	-e SSH_AUTH_SOCK=/ssh-agent \
	--device /dev/net/tun:/dev/net/tun \
	--cap-add=NET_ADMIN \
	--workdir=$(pwd) \
	ttos-build:latest kas shell ttos-dev.yml
