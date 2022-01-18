# Etcd-walker
_Simple CLI tool for etcd_

building:

```
go build
```
in some cases (for example for running inside containers) need to build statically without dependency on libc:
```
go build -ldflags "-linkmode external -extldflags -static"
```

![etcd walker](https://github.com/nexusriot/etcd-walker/blob/main/walker.png?raw=true)