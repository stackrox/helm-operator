# helm-operator

![Build Status](https://github.com/stackrox/helm-operator/workflows/CI/badge.svg?branch=main)

Experimental refactoring of the operator-framework's helm operator.

## Why a fork?

The Helm operator type automates Helm chart operations
by mapping the [values](https://helm.sh/docs/chart_template_guide/values_files/) of a Helm chart exactly to a 
`CustomResourceDefinition` and defining its watched resources in a `watches.yaml` 
[configuration](https://sdk.operatorframework.io/docs/building-operators/helm/tutorial/#watch-the-nginx-cr) file.

For creating a [Level II+](https://sdk.operatorframework.io/docs/advanced-topics/operator-capabilities/operator-capabilities/) operator 
that reuses an already existing Helm chart, we need a [hybrid](https://github.com/operator-framework/operator-sdk/issues/670)
between the Go and Helm operator types is.

The hybrid approach allows adding customizations to the Helm operator, such as:
- value mapping based on cluster state, or 
- executing code in specific events.

## Quick start

- Add this module as a replace directive to your `go.mod`:

  ```
  go mod edit -replace=github.com/joelanford/helm-operator=github.com/stackrox/helm-operator@main
  ```

  For example:

  ```go
  chart, err := loader.Load("path/to/chart")
  if err != nil {
     panic(err)
  }

  reconciler := reconciler.New(
     reconciler.WithChart(*chart),
     reconciler.WithGroupVersionKind(gvk),
  )

  if err := reconciler.SetupWithManager(mgr); err != nil {
     panic(fmt.Sprintf("unable to create reconciler: %s", err))
  }
  ```

