## Etcd-walker
_Simple CLI tool for etcd_

![etcd walker](https://github.com/nexusriot/etcd-walker/blob/main/etcd-walker.gif?raw=true)

### **Building:**

```
go build
```
in some cases (for example for running inside containers) need to build statically without dependency on libc:
```
go build -ldflags "-linkmode external -extldflags -static"
```
to check for lib usage please use ldd command
```
ldd etcd-walker 
```

building 32-bit binary
```
GOOS=linux GOARCH=386 go build -o  etcd-walker_linux_i686 main.go
```

### **Building deb package**

installing required tools including golang
```
sudo apt-get install git devscripts build-essential lintian upx-ucl golang
```
run build script
```
./build-deb.sh
```


### **Running:**
```
./etcd-walker [-host host] [-port port] [-debug]
```

Default values are: **localhost** for host, **2379** for port, debug is **false**

### **Starting etcd for development/testing**

to start etcd as a Docker container: 

```
docker run -d --restart unless-stopped -p 2379:2379 --name etcd quay.io/coreos/etcd:v3.3.27 /usr/local/bin/etcd -advertise-client-urls http://0.0.0.0:2379 -listen-client-urls http://0.0.0.0:2379
```
testing:
```
curl -L http://localhost:2379/v2/keys/test -XPUT -d value="test value"
```
