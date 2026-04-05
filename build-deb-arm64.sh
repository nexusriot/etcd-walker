#!/bin/env bash

dir="$(cd "$(dirname "$0")" && pwd)"
exec "$dir/build-deb.sh" arm64
