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
#   source     built from CDI_REPO at CDI_REF and pushed to the kubevirtci registry

CDI_VERSION="${CDI_VERSION:-}"
CDI_REPO="${CDI_REPO:-https://github.com/kubevirt/containerized-data-importer}"
CDI_REF="${CDI_REF:-main}"

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

  if [[ ${version} == "source" ]]; then
    echo "${_cdi_src_dir}/_out/manifests/release"
    return
  fi

  if [[ ${version} == "nightly" || ${version} == "latest" ]]; then
    version=$(curl -sfL "${_cdi_nightly_base}/latest")
  fi

  if [[ ${version} =~ ^[0-9]{8}$ ]]; then
    echo "${_cdi_nightly_base}/${version}"
  elif [[ ${version} =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
    echo "${_cdi_release_base}/${version}"
  else
    echo "unsupported CDI_VERSION '${CDI_VERSION}', expected a release tag (v1.65.0), a nightly date (20260903), 'nightly' or 'source'" >&2
    return 1
  fi
}

# Check out CDI_REF of CDI_REPO, reusing an earlier checkout of the same ref.
function cdi::fetch_source() {
  if [[ -f ${_cdi_src_dir}/.ref && $(cat "${_cdi_src_dir}/.ref") == "${CDI_REPO}@${CDI_REF}" ]]; then
    return
  fi

  rm -rf "${_cdi_src_dir}"
  git init -q "${_cdi_src_dir}"
  git -C "${_cdi_src_dir}" fetch -q --depth 1 "${CDI_REPO}" "${CDI_REF}"
  git -C "${_cdi_src_dir}" checkout -q FETCH_HEAD
  echo "${CDI_REPO}@${CDI_REF}" > "${_cdi_src_dir}/.ref"
}

# Build CDI from source and push its images to the kubevirtci registry. The manifests
# reference the registry the way the cluster nodes reach it.
function cdi::build_source() {
  local registry=$1

  cdi::fetch_source

  # CDI's build containers chown their output to the calling user unless told they run in
  # podman, where that chown maps to a different host user. Detect the runtime like CDI does.
  if [[ -z ${KUBEVIRTCI_RUNTIME:-} ]] && ! docker ps >/dev/null 2>&1 && podman ps >/dev/null 2>&1; then
    export KUBEVIRTCI_RUNTIME=podman
  fi

  echo "building cdi from ${CDI_REPO}@${CDI_REF}..."
  DOCKER_PREFIX=registry:5000 DOCKER_TAG=devel PULL_POLICY=Always make -C "${_cdi_src_dir}" manifests
  DOCKER_PREFIX="${registry}" DOCKER_TAG=devel make -C "${_cdi_src_dir}" push
}

# Install CDI_VERSION if requested, then wait for CDI and point it at the kubevirtci registry.
# registry is where the kubevirtci registry is reachable from this host, for a source build.
function cdi::deploy() {
  local kubectl=$1
  local registry=$2

  if [[ ${CDI_VERSION} == "source" ]]; then
    cdi::build_source "${registry}"
  fi

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

_cdi_src_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/_out/cdi-src"

# Resolved at source time so an unusable CDI_VERSION fails before the cluster comes up.
if [[ -n ${CDI_VERSION} ]]; then
  _cdi_manifest_base=$(cdi::manifest_base) || exit 1
fi
