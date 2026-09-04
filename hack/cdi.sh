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
# Sourced by hack/kubevirtci.sh and hack/kubevirt.sh.
#
# Set CDI_VERSION to install a specific CDI instead of the one bundled with
# kubevirtci. Accepted forms:
#
#   v1.65.0    a release tag, from the GitHub release assets
#   20260903   a nightly date, from the kubevirt-prow bucket
#   nightly    the most recent nightly

CDI_VERSION="${CDI_VERSION:-}"

_cdi_nightly_base="https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/containerized-data-importer"
_cdi_release_base="https://github.com/kubevirt/containerized-data-importer/releases/download"

# Whether kubevirtci should deploy its bundled CDI. False when we install our own,
# so the CDI operator never sees a version transition it may reject.
function cdi::deploy_bundled() {
  if [[ -n ${CDI_VERSION} ]]; then
    echo "false"
  else
    echo "true"
  fi
}

# Base URL the cdi-operator.yaml and cdi-cr.yaml manifests are fetched from.
function cdi::manifest_base() {
  local version="${CDI_VERSION}"

  if [[ ${version} == "nightly" || ${version} == "latest" ]]; then
    version=$(curl -sfL "${_cdi_nightly_base}/latest")
  fi

  if [[ ${version} =~ ^[0-9]{8}$ ]]; then
    echo "${_cdi_nightly_base}/${version}"
  elif [[ ${version} =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
    echo "${_cdi_release_base}/${version}"
  else
    echo "unsupported CDI_VERSION '${CDI_VERSION}', expected a release tag (v1.65.0), a nightly date (20260903) or 'nightly'" >&2
    return 1
  fi
}

# Install CDI_VERSION if requested, then wait for CDI and point it at the kubevirtci registry.
function cdi::deploy() {
  local kubectl=$1

  if [[ -n ${CDI_VERSION} ]]; then
    echo "installing cdi ${CDI_VERSION} from ${_cdi_manifest_base}..."
    ${kubectl} apply -f "${_cdi_manifest_base}/cdi-operator.yaml"
    ${kubectl} wait --for condition=established --timeout=2m crd/cdis.cdi.kubevirt.io
    ${kubectl} apply -f "${_cdi_manifest_base}/cdi-cr.yaml"
  fi

  echo "waiting for cdi to become ready, this can take a few minutes..."
  ${kubectl} wait cdis.cdi.kubevirt.io/cdi --for condition=Available --timeout=10m

  echo "adding kubevirtci registry to cdi-insecure-registries"
  ${kubectl} patch cdis/cdi --type merge -p '{"spec": {"config": {"insecureRegistries": ["registry:5000"]}}}'
}

# Resolved at source time so an unusable CDI_VERSION fails before the cluster comes up.
if [[ -n ${CDI_VERSION} ]]; then
  _cdi_manifest_base=$(cdi::manifest_base) || exit 1
fi
