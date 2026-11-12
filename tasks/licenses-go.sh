#!/bin/bash

./tools/go-licenses check ./... \
  --ignore "github.com/edorid/docker-machine-driver-pve" \
  --disallowed_types=forbidden,restricted,reciprocal,unknown
