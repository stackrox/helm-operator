# helm-operator

[![Build Status](https://github.com/stackrox/helm-operator/workflows/CI/badge.svg?branch=main)

Experimental refactoring of the operator-framework's helm operator

### Why a fork?

As the helm-operator is an experimental refactoring and not actively maintained we started a fork to 
further support [hybrid operators](https://github.com/operator-framework/operator-sdk/issues/670) based on Helm.

This fork should used as a library and not is recommended to watch CustomResources by configured `watches`.

### Quickstart

Add this lib as a replace directive to your `go.mod`:

```
replace(
    github.com/joelanford/helm-operator => github.com/stackrox/helm-operator
)
```

Example:

```
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

