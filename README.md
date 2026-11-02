# kubernetes-operator

kind create cluster --name observability-local --config kind-config.yaml --wait 5m

kubebuilder init --domain example.com --repo github.com/hdfc/cluster-observability-operator

kubebuilder create api --group observability --version v1alpha1 --kind ClusterObservability

kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml

kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=180s

kubectl apply -f https://github.com/open-telemetry/opentelemetry-operator/releases/latest/download/opentelemetry-operator.yaml

kubectl get pods -n opentelemetry-operator-system

kubectl get crd opentelemetrycollectors.opentelemetry.io