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

# Print the repo@ref recorded for the checkout in dir, nothing if there is none. It is kept in
# .git so the checkout itself stays clean.
function cluster::checked_out_ref() {
  cat "$1/.git/virt-import-ref" 2>/dev/null || true
}

# Check out ref (a branch, tag or commit) of repo into dir, which must not exist. The checkout
# happens in a temporary directory, so a failed fetch leaves no checkout behind.
function cluster::clone() {
  local dir=$1
  local repo=$2
  local ref=$3
  local tmp="${dir}.tmp"

  rm -rf "${tmp}"
  git init -q "${tmp}"
  git -C "${tmp}" fetch -q --depth 1 "${repo}" "${ref}"
  git -C "${tmp}" checkout -q FETCH_HEAD
  echo "${repo}@${ref}" > "${tmp}/.git/virt-import-ref"
  mv "${tmp}" "${dir}"
}

# Check out ref of repo into dir. An existing checkout is reused and never replaced
# automatically, since a running cluster depends on it; down_target is the make target that
# stops that cluster.
function cluster::checkout() {
  local dir=$1
  local repo=$2
  local ref=$3
  local down_target=$4
  local want="${repo}@${ref}"
  local have

  if [[ -d ${dir} ]]; then
    have=$(cluster::checked_out_ref "${dir}")
    if [[ -z ${have} ]]; then
      echo "Reusing ${dir} as it is, it records no ref. Remove it to check out ${want}." >&2
    elif [[ ${have} != "${want}" ]]; then
      echo "${dir} holds ${have}, not ${want}." >&2
      echo "Run 'make ${down_target}' and remove ${dir} to switch." >&2
      exit 1
    fi
    return
  fi

  cluster::clone "${dir}" "${repo}" "${ref}"
}

# Enable a KubeVirt feature gate, keeping the ones already enabled.
function cluster::enable_feature_gate() {
  local kubectl=$1
  local gate=$2
  local gates

  gates=$(${kubectl} get -n kubevirt kubevirt kubevirt -o jsonpath='{.spec.configuration.developerConfiguration.featureGates}')
  if [[ -z ${gates} ]]; then
    ${kubectl} patch -n kubevirt kubevirt kubevirt --type merge \
      -p '{"spec": {"configuration": {"developerConfiguration": {"featureGates": ["'"${gate}"'"]}}}}'
  elif [[ ${gates} != *"\"${gate}\""* ]]; then
    ${kubectl} patch -n kubevirt kubevirt kubevirt --type json \
      -p '[{"op": "add", "path": "/spec/configuration/developerConfiguration/featureGates/-", "value": "'"${gate}"'"}]'
  fi
}
