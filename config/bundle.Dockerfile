FROM scratch
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=aws-efs-csi-driver-operator
LABEL operators.operatorframework.io.bundle.channels.v1=4.11
LABEL operators.operatorframework.io.bundle.channel.default.v1=4.11
COPY manifests/4.11/aws-efs-csi-driver-operator.clusterserviceversion.yaml /manifests/aws-efs-csi-driver-operator.clusterserviceversion.yaml
COPY metadata/annotations.yaml /metadata/annotations.yaml
