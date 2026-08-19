import { useState } from "react";
import { ConfirmDialog } from "./ConfirmDialog";

/** 배포/재배포 확인 문구는 화면마다 동일해야 하므로 한 곳에 고정합니다. (#33) */
export const DEPLOY_CONFIRM_MESSAGE =
  "배포를 진행하시겠습니까? 배포 진행시, 현재 진행중인 pod에 일시적으로 장애가 발생할 수 있습니다.";

/**
 * 배포/재배포 공통 버튼. 누르면 항상 경고 문구로 재확인을 받은 뒤에만 onConfirm을
 * 실행합니다. 배포 계열 위험 동작은 전부 이 컴포넌트를 씁니다.
 */
export function DeployButton({
  label,
  onConfirm,
  busy,
  disabled,
}: {
  label: string;
  onConfirm: () => void | Promise<void>;
  busy?: boolean;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" className="btn btn--warning" onClick={() => setOpen(true)} disabled={disabled || busy}>
        {label}
      </button>
      <ConfirmDialog
        open={open}
        title={label}
        message={DEPLOY_CONFIRM_MESSAGE}
        confirmLabel={label}
        tone="warning"
        busy={busy}
        onCancel={() => setOpen(false)}
        onConfirm={async () => {
          await onConfirm();
          setOpen(false);
        }}
      />
    </>
  );
}
