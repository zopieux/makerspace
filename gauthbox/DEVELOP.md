Cross-compile locally & upload (dropbear: no scp, no rsync):

```shell
env GOOS=linux GOARCH=arm64 go build cmd/authbox/authbox.go
cat authbox | ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ~/.ssh/id_ed25519 root@10.0.0.100 'cat > /tmp/authbox ; chmod +x /tmp/authbox'
```

If not otherwise configured, point `control.shop` to the host (gateway):

```shell
pi$ echo 10.0.0.1 control.shop >> /etc/hosts
```

Run mosquitto server locally:

```shell
$ ./testing/mosquitto.sh
```

Run mock control-command server locally:

```shell
$ ./testing/fake_control.py
```
