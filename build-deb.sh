#!/bin/env bash

version=0.3.2

echo "building deb for etcd-walker $version"

if ! type "dpkg-deb" > /dev/null; then
  echo "please install required build tools first"
fi

project="etcd-walker_${version}_amd64"
folder_name="build/$project"
echo "crating $folder_name"
mkdir -p $folder_name
cp -r DEBIAN/ $folder_name
bin_dir="$folder_name/usr/bin"
mkdir -p $bin_dir
go build -ldflags "-linkmode external -extldflags -static" -o etcd-walker cmd/etcd-walker/main.go

mv etcd-walker $bin_dir
sed -i "s/_version_/$version/g" $folder_name/DEBIAN/control

cd build/ && dpkg-deb --build --root-owner-group $project
