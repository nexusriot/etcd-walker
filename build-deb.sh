#!/bin/env bash

# Single source of truth: pkg/version/version.go (DESIGN §9.3).
version=$(sed -n 's/^var Version = "\(.*\)"/\1/p' pkg/version/version.go)
arch="${1:-amd64}"

echo "building deb for etcd-walker $version ($arch)"

if ! type "dpkg-deb" > /dev/null; then
  echo "please install required build tools first"
fi

case "$arch" in
  amd64)  goarch="amd64" ;;
  arm64)  goarch="arm64" ;;
  *)      echo "unsupported architecture: $arch"; exit 1 ;;
esac

project="etcd-walker_${version}_${arch}"
folder_name="build/$project"
echo "crating $folder_name"
mkdir -p $folder_name
cp -r DEBIAN/ $folder_name
bin_dir="$folder_name/usr/bin"
mkdir -p $bin_dir
if [ "$arch" = "$(go env GOARCH)" ]; then
  go build -ldflags "-linkmode external -extldflags -static -X github.com/nexusriot/etcd-walker/pkg/version.Version=$version" -o etcd-walker cmd/etcd-walker/main.go
else
  CGO_ENABLED=0 GOARCH=$goarch go build -ldflags "-s -w -X github.com/nexusriot/etcd-walker/pkg/version.Version=$version" -o etcd-walker cmd/etcd-walker/main.go
fi

mv etcd-walker $bin_dir
sed -i "s/_version_/$version/g" $folder_name/DEBIAN/control
sed -i "s/Architecture: .*/Architecture: $arch/" $folder_name/DEBIAN/control

cd build/ && dpkg-deb --build -Z gzip --root-owner-group $project
