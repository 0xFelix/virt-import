# Demo: export a VM and import it again

This walkthrough exports a Fedora VM with two disks to an OCI artifact, pushes it to a registry and
imports it as a new VM with virt-import. It runs on a local kubevirtci cluster with KubeVirt and CDI
built from the commits virt-import needs, so no existing cluster is required.

The VM gets a Fedora root disk and a blank data disk. A file written to each disk before the export
must show up in the imported VM.

## Prerequisites

- Linux with podman, `make`, Go, `git`
- `kubectl`, [skopeo](https://github.com/containers/skopeo) and `jq`
- An SSH key pair, `~/.ssh/id_rsa` below

## 1. Start the cluster

```shell
make kubevirt-up    # kubevirtci, KubeVirt and CDI built from source, cert-manager
make kubevirt-sync  # build and deploy virt-import
make build          # bin/virtimportctl
```

The first `kubevirt-up` builds KubeVirt and CDI and takes a while. Set up the shell for the next
steps:

```shell
export KUBECONFIG=$(./hack/kubevirt.sh kubeconfig 2>/dev/null)
REG=$(./hack/kubevirt.sh registry 2>/dev/null)  # the cluster registry as your machine reaches it
VIRTCTL=$PWD/_kubevirt/_out/cmd/virtctl/virtctl
vmssh() {
  $VIRTCTL ssh -n demo -i ~/.ssh/id_rsa \
    -t -oStrictHostKeyChecking=no -t -oUserKnownHostsFile=/dev/null "$@"
}
kubectl create namespace demo
```

Inside the cluster the same registry is `registry:5000`. It is listed in CDI's insecure registries,
so it needs no credentials or CA.

## 2. Create the source VM

```shell
$VIRTCTL create vm --name fedora \
  --volume-import=type:registry,url:docker://quay.io/containerdisks/fedora:latest,size:10Gi,name:rootdisk \
  --volume-import=type:blank,size:1Gi,name:datadisk \
  --instancetype=u1.small --preference=fedora \
  --ssh-key="$(<~/.ssh/id_rsa.pub)" |
  kubectl apply -n demo -f -
kubectl wait -n demo vm/fedora --for=condition=Ready --timeout=10m
until vmssh fedora@vm/fedora -c true 2>/dev/null; do sleep 5; done
```

Write a file to the root disk, and format the data disk and write a file to it:

```shell
vmssh fedora@vm/fedora -c '
  echo "rootdisk $(date)" | sudo tee /root-test.txt
  sudo mkfs.ext4 -q /dev/vdb && sudo mount /dev/vdb /mnt
  echo "datadisk $(date)" | sudo tee /mnt/data-test.txt
  sudo umount /mnt'
```

`/dev/vdb` is the data disk, check with `lsblk` if in doubt.

## 3. Export the VM

Stop the VM so its disks are consistent, then export it as an OCI archive:

```shell
$VIRTCTL stop -n demo fedora
kubectl wait -n demo vmi/fedora --for=delete --timeout=5m
$VIRTCTL vmexport create fedora-export -n demo --vm=fedora
$VIRTCTL vmexport download fedora-export -n demo --format=oci --output=fedora.oci.tar --port-forward
$VIRTCTL vmexport delete fedora-export -n demo
tar -tf fedora.oci.tar
```

The archive is an OCI image layout. Its blobs are the image index, the manifest, the VM definition
and one zstd-compressed layer per disk, named after the VM volume.

## 4. Push it to the registry

```shell
podman unshare skopeo copy --multi-arch=all --dest-tls-verify=false \
  oci-archive:fedora.oci.tar docker://$REG/demo/fedora:v1
skopeo inspect --raw --tls-verify=false docker://$REG/demo/fedora:v1 | jq
```

`podman unshare` lets skopeo set the file owners recorded in the archive while unpacking it without
being root. The registry now holds an image index with one manifest per architecture, here
`amd64`.

## 5. Import it

```shell
bin/virtimportctl create fedora-import -n demo \
  --url docker://registry:5000/demo/fedora:v1 --target-name fedora-copy
kubectl get vmimport -n demo -w
```

The `REASON` column moves through `FetchingMetadata`, `ImportingDisks` and `CreatingTarget` to
`ImportComplete`. Meanwhile:

```shell
kubectl get vmimport fedora-import -n demo -o jsonpath='{.status}' | jq  # artifact and disk progress
kubectl get vm,dv,pvc -n demo
```

The import is the same as this manifest:

```yaml
apiVersion: import.kubevirt.io/v1alpha1
kind: VirtualMachineImport
metadata:
  name: fedora-import
  namespace: demo
spec:
  source:
    registry:
      url: docker://registry:5000/demo/fedora:v1
  targetName: fedora-copy
```

## 6. Check the imported VM

```shell
$VIRTCTL start -n demo fedora-copy
kubectl wait -n demo vm/fedora-copy --for=condition=Ready --timeout=10m
until vmssh fedora@vm/fedora-copy -c true 2>/dev/null; do sleep 5; done
vmssh fedora@vm/fedora-copy -c '
  cat /root-test.txt
  sudo mount /dev/vdb /mnt && cat /mnt/data-test.txt && sudo umount /mnt'
```

Both files are there, with the timestamps written before the export. The imported disks are owned
by the new VM: deleting it deletes its DataVolumes and PVCs, while deleting the
`VirtualMachineImport` only removes the CR.

```shell
kubectl get dv -n demo -o 'custom-columns=NAME:.metadata.name,OWNER:.metadata.ownerReferences[0].name'
```

## 7. Clean up

```shell
kubectl delete namespace demo
make kubevirt-down
```
