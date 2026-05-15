#!/usr/bin/env bash

docker run \
	--rm \
	-it \
	-v $(pwd):$(pwd) \
	-v $SSH_AUTH_SOCK:/ssh-agent \
	-e SSH_AUTH_SOCK=/ssh-agent \
	--workdir $(pwd) \
	ttos-build:latest kas build ttos-dev.yml
