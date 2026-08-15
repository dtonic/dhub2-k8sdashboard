package v1

const Version uint32 = 1

const (
	KindPod         = "Pod"
	KindNode        = "Node"
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
	KindCronJob     = "CronJob"
	KindReplicaSet  = "ReplicaSet"
	KindEvent       = "Event"
)
