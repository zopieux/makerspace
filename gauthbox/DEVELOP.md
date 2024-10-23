```
cat buttonless | ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ~/.ssh/id_ed25519 root@10.0.0.100 'cat > /tmp/buttonless'
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ~/.ssh/id_ed25519 root@10.0.0.100 chmod +x /tmp/buttonless
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ~/.ssh/id_ed25519 root@10.0.0.100 /tmp/buttonless 
```

```
( cd .test && python -m http.server 8000 )
```
