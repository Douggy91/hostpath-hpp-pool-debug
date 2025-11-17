podman build -t {your_registry}/{your_repository}:{tags}
podman push {your_registry}/{your_repository}:{tags}

make generate
make manifests
make deploy IMG={your_registry}/{your_repository}:{tags}

oc apply -f config/samples/debugger_v1beta1_debugger.yaml
