package v1

const Version uint32 = 1

const (
	KindNamespace   = "Namespace"
	KindPod         = "Pod"
	KindNode        = "Node"
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
	KindCronJob     = "CronJob"
	KindReplicaSet  = "ReplicaSet"
	KindEvent       = "Event"
)
