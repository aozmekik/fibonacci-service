#!/usr/bin/env bash

# Runs the service on a local Talos cluster in Docker and checks it there.
#
# The image is built from the working tree and pushed to a local registry,
# under the tag the manifest asks for. The nodes get a registry mirror for
# ghcr.io which points at that registry, so the manifest is applied exactly
# the way it is in the repository.
#
# Requires docker, talosctl, kubectl and GNU make 4, which is MAKE=gmake on
# macOS, and a Docker host with the br_netfilter module loaded. The image is
# pushed by the buildx builder to a registry on 127.0.0.1, so a builder with
# the docker-container driver has to be on the host network, which is
# --driver-opt network=host. Nothing is written outside of a temporary
# directory, and the cluster and the registry are removed at the end. Set
# KEEP=1 to keep them.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

MAKE=${MAKE:-make}
MANIFEST=deploy/fibonacci-service.yaml
CLUSTER=fibonacci-service-test
REGISTRY=${CLUSTER}-registry
CLIENT=fibonacci-service-test-client
# not the subnet talosctl takes by default, so that a cluster which has it can keep running
SUBNET=10.5.100.0/24
# the gateway of the subnet, which is how the nodes reach the ports published on the host
GATEWAY=10.5.100.1

# The nodes are containers, so they run on the kernel of the Docker host and can not load
# a module of their own. Without br_netfilter the answer of a pod to another pod of its node
# goes over the bridge and past the rules of kube-proxy, and a service is not reachable
# from the node its pods are on. Docker does not load the module itself since version 28.
if ! docker run --rm --network host --entrypoint test registry:3 -e /proc/sys/net/bridge/bridge-nf-call-iptables; then
  echo "FAIL: the Docker host has no br_netfilter module loaded, run there: sudo modprobe br_netfilter"

  exit 1
fi

STATE=$(mktemp -d)

export KUBECONFIG=${STATE}/kubeconfig
export TALOSCONFIG=${STATE}/talosconfig

function cleanup {
  kill "${LOAD_PID:-}" 2>/dev/null && wait "${LOAD_PID}" 2>/dev/null || true

  if [[ -n "${KEEP:-}" ]]; then
    echo "keeping the cluster, KUBECONFIG=${KUBECONFIG} TALOSCONFIG=${TALOSCONFIG}"

    return
  fi

  talosctl cluster destroy --name "${CLUSTER}" --state "${STATE}/clusters" || true
  docker rm --force --volumes "${REGISTRY}" >/dev/null || true
  rm -rf "${STATE}"
}

trap cleanup EXIT

# get calls the service from a pod in the cluster and prints the body of the response
function get {
  kubectl exec "${CLIENT}" -- curl --silent --max-time 10 "http://${SERVICE}$1" || true
}

function expect {
  local actual

  actual=$(get "$1")

  if [[ "${actual}" != *"$2"* ]]; then
    echo "FAIL: GET $1: expected $2, got ${actual}"

    exit 1
  fi

  echo "ok: GET $1 -> ${actual}"
}

IMAGE_TAG=$(sed -n 's|^ *image: ghcr.io/.*:\(.*\)$|\1|p' "${MANIFEST}")
# the revision the Makefile builds into the image, which the pods are asked for below
REVISION=$(git describe --match=none --always --abbrev=8 --dirty)

# Docker picks the port on the host, so that the registry does not need one which another registry may have taken
docker run --detach --name "${REGISTRY}" --publish 5000 registry:3 >/dev/null

REGISTRY_PORT=$(docker port "${REGISTRY}" 5000/tcp | sed -n '1s/.*://p')

"${MAKE}" image-fibonacci-service REGISTRY="127.0.0.1:${REGISTRY_PORT}" IMAGE_TAG="${IMAGE_TAG}" PUSH=true \
  PLATFORM="linux/$(docker info --format '{{.Architecture}}' | sed 's/x86_64/amd64/; s/aarch64/arm64/')"

cat >"${STATE}/mirror.yaml" <<EOF
apiVersion: v1alpha1
kind: RegistryMirrorConfig
name: ghcr.io
endpoints:
  - url: http://${GATEWAY}:${REGISTRY_PORT}
EOF

talosctl cluster create docker \
  --name "${CLUSTER}" \
  --state "${STATE}/clusters" \
  --subnet "${SUBNET}" \
  --config-patch "@${STATE}/mirror.yaml"

# a warning here would come from the pod security admission, which Talos sets to warn on "restricted"
applied=$(kubectl apply --filename "${MANIFEST}" 2>&1) || {
  echo "${applied}"

  exit 1
}

echo "${applied}"

if [[ "${applied}" == *Warning* ]]; then
  echo "FAIL: the manifest was applied with warnings"

  exit 1
fi

kubectl rollout status deployment/fibonacci-service --timeout=5m

# The requests are sent by a pod, to the cluster IP of the service. This is the path
# kube-proxy routes, and the one a pod has to be taken out of before it stops.
kubectl apply --filename - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${CLIENT}
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: curl
      image: curlimages/curl:8.11.1
      command: ["sleep", "3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
EOF

kubectl wait --for=condition=Ready "pod/${CLIENT}" --timeout=5m

# the address and not the name, so that the DNS of the cluster is not checked along with the service
SERVICE=$(kubectl get service fibonacci-service --output jsonpath='{.spec.clusterIP}')

# The pods have to run the image which was built above, and not the release with the same tag.
# A node tries the mirror first, but if it does not get the image there it pulls it from ghcr.io,
# and everything below would pass on that one.
#
# The addresses are read before the loop. In the list of the loop a kubectl which fails
# would leave nothing to loop over, and the pods would count as checked.
ips=$(kubectl get pods --selector app.kubernetes.io/name=fibonacci-service --output jsonpath='{.items[*].status.podIP}')

if [[ -z "${ips}" ]]; then
  echo "FAIL: there are no pods to ask for their revision"

  exit 1
fi

for ip in ${ips}; do
  metrics=$(kubectl exec "${CLIENT}" -- curl --silent --max-time 10 "http://${ip}:2122/metrics" || true)

  if [[ "${metrics}" != *"revision=\"${REVISION}\""* ]]; then
    echo "FAIL: the pod at ${ip} does not run the image which was built here at ${REVISION}"

    exit 1
  fi
done

echo "ok: the pods run the image which was built here at ${REVISION}"

expect "/v1/fibonacci?n=10" "[0,1,1,2,3,5,8,13,21,34]"
expect "/v1/fibonacci?n=0" "[]"
expect "/v1/fibonacci?n=-1" "n must not be negative"
expect "/v1/fibonacci?n=1001" "n must not be greater than 1000"

# replace every pod while requests keep coming, each on a connection of its own: none of them may fail
# shellcheck disable=SC2016 # the loop is expanded by the shell of the pod, which gets the address as $0
kubectl exec "${CLIENT}" -- sh -c '
  ok=0 failed=0 codes=

  until [ -e /tmp/stop ]; do
    body=$(curl --silent --max-time 5 "http://$0/v1/fibonacci?n=5") && code=0 || code=$?

    if [ "${body}" = "[0,1,1,2,3]" ]; then
      ok=$((ok + 1))
    else
      failed=$((failed + 1)) codes="${codes} ${code}"
    fi
  done

  echo "${ok} ${failed}${codes}"
' "${SERVICE}" >"${STATE}/requests" &
LOAD_PID=$!

generation=$(kubectl get pods --selector app.kubernetes.io/name=fibonacci-service \
  --output jsonpath='{.items[0].metadata.labels.pod-template-hash}')

kubectl rollout restart deployment/fibonacci-service
kubectl rollout status deployment/fibonacci-service --timeout=5m

# The pods of the previous generation are gone only after their preStop delay and their shutdown.
# A kubectl which fails is asked again, as its empty output would look like no pods being left.
until old=$(kubectl get pods --selector "pod-template-hash=${generation}" --output name) && [[ -z "${old}" ]]; do
  sleep 1
done

kubectl exec "${CLIENT}" -- touch /tmp/stop
wait "${LOAD_PID}"

read -r ok failed codes <"${STATE}/requests"

if [[ "${failed}" != 0 || "${ok}" == 0 ]]; then
  echo "FAIL: ${failed} of $((ok + failed)) requests failed during the rollout, curl exited with: ${codes}"

  exit 1
fi

echo "ok: ${ok} requests during the rollout, none failed"
