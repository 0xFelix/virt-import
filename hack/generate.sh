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

set -e

_base_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
_client_staging_dir="${_base_dir}/staging/src/kubevirt.io/virt-import-client-go"
_client_gen=$(GOWORK=off go -C "${_base_dir}/tools" tool -n client-gen)

"${_client_gen}" \
  --clientset-name virtimport \
  --input-base kubevirt.io/virt-import-api \
  --input core/v1alpha1 \
  --output-dir "${_client_staging_dir}" \
  --output-pkg kubevirt.io/virt-import-client-go \
  --go-header-file "${_base_dir}/hack/boilerplate.go.txt"
