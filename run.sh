#!/bin/bash

set -euo pipefail

image="${1:?usage: ./run.sh IMAGE [command...]}"
shift

exec docker run --rm -it  -v "$PWD:/work"  "$image"
