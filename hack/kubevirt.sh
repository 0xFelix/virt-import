#!/bin/bash
#
# This file is part of the KubeVirt project
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Copyright The KubeVirt Authors.
#

set -ex

export KUBEVIRT_MEMORY_SIZE="${KUBEVIRT_MEMORY_SIZE:-16G}"
export KUBEVIRT_STORAGE="${KUBEVIRT_STORAGE:-rook-ceph-default}"
# KubeVirt is built from KUBEVIRT_REF (a branch, tag or commit) of KUBEVIRT_REPO.
export KUBEVIRT_REPO="${KUBEVIRT_REPO:-https://github.com/kubevirt/kubevirt.git}"
export KUBEVIRT_REF="${KUBEVIRT_REF:-main}"

_base_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
_kubevirt_dir="${_base_dir}/_kubevirt"
_kubectl="${_base_dir}/_kubevirt/kubevirtci/cluster-up/kubectl.sh"
_kubessh="${_base_dir}/_kubevirt/kubevirtci/cluster-up/ssh.sh"
_kubevirtcicli="${_base_dir}/_kubevirt/kubevirtci/cluster-up/cli.sh"
_action=$1
shift

source "${_base_dir}/hack/cluster.sh"
source "${_base_dir}/hack/cdi.sh"
KUBEVIRT_DEPLOY_CDI="$(cdi::deploy_bundled)"
export KUBEVIRT_DEPLOY_CDI

function kubevirt::up() {
  make cluster-up -C "${_kubevirt_dir}"
  make cluster-sync -C "${_kubevirt_dir}"
  KUBECONFIG=$(kubevirt::kubeconfig)
  export KUBECONFIG

  # KubeVirt only patches insecureRegistries when it deploys CDI itself, so do it here.
  cdi::deploy "${_kubectl}" "$(kubevirt::registry)"

  # Get the default storage class to patch vmStateStorageClass
  DEFAULT_STORAGE_CLASS=$(${_kubectl} get storageclass -o jsonpath='{.items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")].metadata.name}')
  if [ -n "$DEFAULT_STORAGE_CLASS" ]; then
    # Patch kubevirt with VM state storage class
    echo "patching kubevirt with vmStateStorageClass ${DEFAULT_STORAGE_CLASS}"
    ${_kubectl} patch -n kubevirt kubevirt kubevirt --type merge -p '{"spec": {"configuration": { "vmStateStorageClass": "'"$DEFAULT_STORAGE_CLASS"'" }}}'
  fi

  # The functional tests import VMs exported as OCI artifacts.
  cluster::enable_feature_gate "${_kubectl}" OCIExport

  echo "waiting for kubevirt to become ready, this can take a few minutes..."
  ${_kubectl} -n kubevirt wait kv kubevirt --for condition=Available --timeout=15m
}

function kubevirt::down() {
  make cluster-down -C "${_kubevirt_dir}"
}

function kubevirt::kubeconfig() {
  "${_kubevirt_dir}/kubevirtci/cluster-up/kubeconfig.sh"
}

function kubevirt::registry() {
  port=$(${_kubevirtcicli} ports registry 2>/dev/null)
  echo "localhost:${port}"
}

# Only bringing a cluster up checks out KubeVirt; every other action works on the existing
# checkout, whatever ref it holds.
if [[ ${_action} == "up" ]]; then
  cluster::checkout "${_kubevirt_dir}" "${KUBEVIRT_REPO}" "${KUBEVIRT_REF}" kubevirt-down
elif [[ ! -d ${_kubevirt_dir} ]]; then
  echo "${_kubevirt_dir} does not exist, run 'make kubevirt-up' first." >&2
  exit 1
fi

case ${_action} in
  "up")
    kubevirt::up
    ;;
  "down")
    kubevirt::down
    ;;
  "kubeconfig")
    kubevirt::kubeconfig
    ;;
  "registry")
    kubevirt::registry
    ;;
  "ssh")
    ${_kubessh} "$@"
    ;;
  "kubectl")
    ${_kubectl} "$@"
    ;;
  *)
    echo "No command provided, known commands are 'up', 'down', 'kubeconfig', 'registry', 'ssh' and 'kubectl'"
    exit 1
    ;;
esac
