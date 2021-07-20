# aws-efs-csi-driver operator

An operator to deploy the [AWS EFS CSI driver](https://github.com/openshift/aws-efs-csi-driver) in OKD.

This operator is installed by OLM.

# Quick start

To build and run the operator locally:

```shell
# Create only the resources the operator needs to run via CLI
oc apply -f - <<EOF
apiVersion: operator.openshift.io/v1
kind: ClusterCSIDriver
metadata:
    name: efs.csi.aws.com
spec:
  logLevel: Normal
  managementState: Managed
  operatorLogLevel: Trace
EOF

# Build the operator
make

# Set the environment variables
export DRIVER_IMAGE=amazon/aws-efs-csi-driver:v1.1.1
export NODE_DRIVER_REGISTRAR_IMAGE=quay.io/openshift/origin-csi-node-driver-registrar:latest
export LIVENESS_PROBE_IMAGE=quay.io/openshift/origin-csi-livenessprobe:4.8
export OPERATOR_NAME=aws-efs-csi-driver-operator

# Run the operator via CLI
./aws-efs-csi-driver-operator start --kubeconfig $KUBECONFIG --namespace openshift-cluster-csi-drivers
```


# OLM

To build an bundle + index images, use `hack/create-bundle`.

```shell
cd hack
./create-bundle quay.io/openshift/origin-aws-efs-csi-driver:latest quay.io/openshift/origin-aws-efs-csi-driver-operator:latest quay.io/<my-repo>/efs-bundle quay.io/<my-repo>/efs-index
```

At the end it will print a command that creates `Subscription` for the newly created index image.
