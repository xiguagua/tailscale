#!/usr/bin/env sh

set -eu

REPO="$REPO"
OAUTH_CLIENT_ID="$OAUTH_CLIENT_ID"
OAUTH_CLIENT_SECRET="$OAUTH_CLIENT_SECRET"

TAG=$(eval `./tool/go run ./cmd/mkversion`)

make kube-generate-all # ensure things are up to date

# TODO: add a way to build images locally and load into a kind cluster
REPO="${REPO}/proxy" TAGS="${TAG}" make publishdevimage
REPO="${REPO}/operator" TAGS="${TAG}" make publishdevoperator

kubectl apply -f ./cmd/k8s-operator/deploy/crds/

helm upgrade \
  --install \
    operator ./cmd/k8s-operator/deploy/chart \
  --namespace tailscale \
  --create-namespace \
  --set operator.image.repo="${REPO}/operator" \
  --set operator.image.tag="${TAG} \
  --set proxy.image.repo="${REPO}/proxy \
  --set proxy.image.tag="${TAG}" \
  --set installCRDs=false \
  --set-string apiServerProxyConfig.mode="true" \
  --set oauth.clientId="${OAUTH_CLIENT_ID}" \
  --set oauth.clientSecret="${OAUTH_CLIENT_SECRET}" \
  --set operatorConfig.logging=debug \

## ingress-nginx is used in tests

helm upgrade --install ingress ingress-nginx/ingress-nginx