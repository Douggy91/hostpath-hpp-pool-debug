package controller // 패키지 이름을 'controller'로 통일

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	// 프로젝트 경로에 맞춰 수정
	debuggerv1beta1 "test.local/hpp-pool-debug-operator/api/v1beta1" 
)

// DebuggerReconciler reconciles a Debugger object
type DebuggerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=debugger.test.local,resources=debuggers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

// Reconcile 함수: Operator의 핵심 조정 루프 (지정된 Pod의 상태를 주기적으로 확인)
func (r *DebuggerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// A. CR 인스턴스 조회
	debugger := &debuggerv1beta1.Debugger{}
	if err := r.Get(ctx, req.NamespacedName, debugger); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// B. 대상 Pod 목록 조회
	selector, err := metav1.LabelSelectorAsSelector(&debugger.Spec.PodSelector)
	if err != nil {
		log.Error(err, "Invalid PodSelector defined in DebuggerSpec")
		return ctrl.Result{}, nil 
	}

	targetPods := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(debugger.Spec.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	}
	
	if err := r.List(ctx, targetPods, listOpts...); err != nil {
		log.Error(err, "Failed to list target Pods")
		return ctrl.Result{}, err
	}

	// C. Crash 감지 및 Debug Pod 생성 시도
	for _, pod := range targetPods.Items {
		if isPodCrashing(&pod) {
			// CrashLoopBackOff 발생 시 Controller Pod 로그로 출력 (요청 사항)
			log.Info("🚨 CRASH DETECTED: CrashLoopBackOff on target Pod", 
                "targetPodName", pod.Name, 
                "node", pod.Spec.NodeName,
                "namespace", pod.Namespace,
            )
			
			// Debug Pod 생성 로직 실행
			if err := r.ensureDebugPod(ctx, debugger, &pod); err != nil {
				log.Error(err, "Failed to ensure Debug Pod", "targetPod", pod.Name)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, err
			}
		}
	}
    
	// D. 주기적인 재큐: 60초마다 Pod 상태를 다시 확인
	// 이 재큐가 Operator가 '모니터링'을 수행하는 핵심 메커니즘입니다.
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

// isPodCrashing: CrashLoopBackOff 상태인지 확인
func isPodCrashing(pod *corev1.Pod) bool {
	if pod.Status.ContainerStatuses == nil {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		// Waiting 상태이고, Waiting 이유가 CrashLoopBackOff인지 확인
		if status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

// ensureDebugPod: Debug Pod 생성 (해당 노드에, 지정된 커맨드로, TARGET_CON 환경 변수 포함)
func (r *DebuggerReconciler) ensureDebugPod(ctx context.Context, debugger *debuggerv1beta1.Debugger, targetPod *corev1.Pod) error {
	log := log.FromContext(ctx)

	// 1. Debug Pod 이름 생성 (고유성 확보)
	debugPodName := fmt.Sprintf("%s-%s-debug", debugger.Name, targetPod.Name)
	debugPod := &corev1.Pod{}

	// 2. 이미 존재하는지 확인
	err := r.Get(ctx, types.NamespacedName{Name: debugPodName, Namespace: debugger.Spec.Namespace}, debugPod)
	if err == nil {
		return nil // 이미 존재하면 아무것도 하지 않음 (Desired State 충족)
	}
	if !apierrors.IsNotFound(err) {
		return err // 기타 에러
	}

	// 3. Debug Pod 객체 정의
	mountPath := debugger.Spec.MountPath
	if mountPath == "" {
                mountPath = "/var/hpvolumes/csi"
        }
	debugCommand := []string{
		"/bin/bash",
		"-c",
		fmt.Sprintf("echo ${TARGET_CON} && oc debug node/${NODE_NAME} -- /bin/bash -c \"chroot /host sh -c 'cp -r /var/hpvolumes/csi/* /home/core/'\"  && oc debug ${TARGET_CON} -- bash -c \"mounter --mountPath %s --hostPath /host --unmount\" &&sleep 60s && oc debug node/${NODE_NAME} -- /bin/bash -c \"chroot /host sh -c 'cp -r /home/core/pvc-* /var/hpvolumes/csi/'\"",mountPath),
	}
	debugImage := debugger.Spec.DebugImage
	if debugImage == "" {
		debugImage = "busybox"
	}
	var targetContainer corev1.Container

	if len(targetPod.Spec.Containers) == 0 {
            // 컨테이너가 없으면 에러를 반환하거나 기본값으로 처리합니다.
            log.Info("Target Pod has no containers to debug", "targetPod", targetPod.Name)
            // VolumeMounts 등을 비워둔 채 진행하거나 여기서 에러를 반환할 수 있습니다.
        } else {
            // 첫 번째 컨테이너를 사용합니다.
            targetContainer = targetPod.Spec.Containers[0]
        }

	newDebugPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: debugPodName,
			Namespace: debugger.Spec.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": debugger.Name,
				"debugger-for-pod": targetPod.Name,
			},
		},
		Spec: corev1.PodSpec{
			// ⭐️ 핵심: 크래시 Pod와 동일한 노드에 생성
			NodeName: targetPod.Spec.NodeName,
		        ServiceAccountName: debugger.Spec.ServiceAccountName,
			Volumes: targetPod.Spec.Volumes,
			Tolerations: targetPod.Spec.Tolerations,
			Affinity: targetPod.Spec.Affinity,
			NodeSelector: targetPod.Spec.NodeSelector,
			Containers: []corev1.Container{
				{
					Name: "debugger-container",
					Image: debugImage,
					VolumeMounts: targetContainer.VolumeMounts,
					SecurityContext: targetContainer.SecurityContext,
					Command: debugCommand,
					Env: []corev1.EnvVar{ // ⭐️ 핵심: 환경 변수 주입
						{
							Name:  "TARGET_CON",
							Value: targetPod.Name, // 크래시 Pod의 이름을 값으로 설정
						},
						{
							Name:  "NODE_NAME",
							Value: targetPod.Spec.NodeName, // 크래시 Pod의 이름을 값으로 설정
						},
						{
							Name:  "COMMAND",
							Value: fmt.Sprintf("echo ${TARGET_CON} && oc debug ${TARGET_CON} -- bash -c 'mounter --mountPath %s --hostPath /host --unmount'",mountPath), // 크래시 Pod의 이름을 값으로 설정
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyOnFailure,
		},
	}

	// 4. 오너십 설정 (CR 삭제 시 Debug Pod도 자동 삭제)
	if err := ctrl.SetControllerReference(debugger, newDebugPod, r.Scheme); err != nil {
		return err
	}

	// 5. 생성
	log.Info("✅ Debug Pod Created with TARGET_CON env", "name", debugPodName, "target_pod", targetPod.Name)
	if err = r.Create(ctx, newDebugPod); err != nil {
		return err
	}

	return nil
}

// SetupWithManager: Controller를 Manager에 등록
func (r *DebuggerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&debuggerv1beta1.Debugger{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

