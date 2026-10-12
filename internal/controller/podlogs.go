/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// maxFetcherLogSize bounds the fetcher logs read. The result line is at most 2 MiB.
const maxFetcherLogSize = 4 * 1024 * 1024

// PodLogReader reads the logs of a Pod container.
type PodLogReader interface {
	GetLogs(ctx context.Context, namespace, name, container string) ([]byte, error)
}

type kubePodLogReader struct {
	client kubernetes.Interface
}

// NewPodLogReader returns a PodLogReader using client.
func NewPodLogReader(client kubernetes.Interface) PodLogReader {
	return &kubePodLogReader{client: client}
}

func (r *kubePodLogReader) GetLogs(ctx context.Context, namespace, name, container string) ([]byte, error) {
	return r.client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{
		Container:  container,
		LimitBytes: ptr.To[int64](maxFetcherLogSize),
	}).DoRaw(ctx)
}
