# Etcd-walker
_Simple CLI tool for etcd_

![etcd walker](https://github.com/nexusriot/etcd-walker/blob/main/etcd-walker.gif?raw=true)

**Building:**

```
go build
```
in some cases (for example for running inside containers) need to build statically without dependency on libc:
```
go build -ldflags "-linkmode external -extldflags -static"
```

**Running:**
```
./etcd-walker [-host host] [-port port] [-debug]
```

Default values are: **localhost** for host, **2379** for port, debug is **false**