# Tools the functional tests run in helper Pods: skopeo, curl, tar and coreutils.
# Only pushed to the kubevirtci registry by cluster-sync and kubevirt-sync, never released.
FROM quay.io/centos/centos:stream10

RUN dnf install -y --setopt=install_weak_deps=False skopeo tar && \
    dnf clean all
