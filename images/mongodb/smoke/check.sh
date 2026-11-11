set -euo pipefail
shell() { mongosh --quiet --port 57017 --eval "$1"; }
ready() { [ "$(shell 'db.runCommand({ ping: 1 }).ok' 2> /dev/null)" = 1 ]; }
for _ in $(seq 60); do ready && break; sleep 1; done
ready
[ "$(shell 'db.version()')" = "$DATABASIFY_RELEASE" ]
[ "$(shell 'db.serverStatus().tcmalloc.usingPerCPUCaches ?? false')" = false ]
shell 'rs.initiate({ _id: "smoke", members: [{ _id: 0, host: "127.0.0.1:57017" }] }).ok' > /dev/null
primary() { [ "$(shell 'db.hello().isWritablePrimary')" = true ]; }
for _ in $(seq 60); do primary && break; sleep 1; done
primary
[ "$(shell 'db.getSiblingDB("smoke").checks.insertOne({ ok: 1 }).acknowledged')" = true ]
databasify ctl is-active --quiet server.service
[ "$(databasify ctl show -p NRestarts --value server)" = 0 ]
databasify ctl logs -u server -n 3
pbm version --short
pbm-agent version --short
pcsm version
mongodump --version | sed -n 1p
mongorestore --version | sed -n 1p
echo "mongodb $DATABASIFY_RELEASE answered through the wrapper as a one-member replica set"
databasify ctl stop server
find /srv/mongodb/data -mindepth 1 -delete
install -d -o mongodb -g mongodb -m 750 /srv/mongodb/tls
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=127.0.0.1 -keyout /tmp/smoke.key -out /srv/mongodb/tls/ca.crt 2> /dev/null
cat /srv/mongodb/tls/ca.crt /tmp/smoke.key > /srv/mongodb/tls/server.pem
chown mongodb:mongodb /srv/mongodb/tls/*
echo '{"databaseId":"smoke","ip":"127.0.0.1","clustered":false,"readOnly":false,"members":[{"ip":"127.0.0.1","position":1,"voting":true,"cpus":1,"memoryMb":2048,"diskGb":10}],"primary":null,"secrets":{"admin":"smoke-admin","replication":"smoke-replication","agent":"smoke-agent"},"settings":{},"archive":null}' | databasify node configure > /dev/null
echo '{}' | databasify node start > /dev/null
echo '{"key":"smoke-ready","expectPrimary":false}' | databasify node ready > /dev/null
for _ in $(seq 120); do echo '{"key":"smoke-ready"}' | databasify operation status | grep -q '"state":"running"' || break; sleep 1; done
echo '{"key":"smoke-ready"}' | databasify operation status | grep -q '"state":"succeeded"'
echo '{"secrets":{"admin":"smoke-admin","replication":"smoke-replication","agent":"smoke-agent"},"account":null}' | databasify node bootstrap > /dev/null
echo '{}' | databasify node status | grep -q '"role":"primary"'
echo "mongodb $DATABASIFY_RELEASE runs as an engine node"
[ "$(id -u mongodb)" = 64999 ] && [ "$(id -g mongodb)" = 64999 ]
