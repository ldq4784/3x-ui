import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useQuery } from '@tanstack/react-query';
import { Alert, Modal, Select, Space, Typography } from 'antd';
import { HttpUtil } from '@/utils';

interface NodeXrayModalProps {
  open: boolean;
  onClose: () => void;
  targetIds: number[];
  targetNames: string[];
  currentVersion?: string;
  onConfirm: (ids: number[], version: string) => Promise<void>;
}

export default function NodeXrayModal({
  open,
  onClose,
  targetIds,
  targetNames,
  currentVersion,
  onConfirm,
}: NodeXrayModalProps) {
  const { t } = useTranslation();
  const [userSelectedVersion, setUserSelectedVersion] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);

  const [prevOpen, setPrevOpen] = useState(false);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (open) {
      setUserSelectedVersion(null);
    }
  }

  const { data: versions = [], isLoading } = useQuery({
    queryKey: ['server', 'xrayVersions'],
    queryFn: async () => {
      const msg = await HttpUtil.get<string[]>('/panel/api/server/getXrayVersion');
      return msg?.success && Array.isArray(msg.obj) ? msg.obj : [];
    },
    staleTime: 5 * 60 * 1000,
    enabled: open,
  });

  const defaultVersion =
    currentVersion && currentVersion !== '-' && currentVersion !== 'Unknown'
      ? currentVersion
      : versions[0] || '';

  const selectedVersion = userSelectedVersion ?? defaultVersion;

  const handleOk = async () => {
    if (!selectedVersion) return;
    setConfirming(true);
    try {
      await onConfirm(targetIds, selectedVersion);
      onClose();
    } finally {
      setConfirming(false);
    }
  };

  const isBatch = targetIds.length > 1;

  return (
    <Modal
      open={open}
      title={t('pages.nodes.adjustXrayTitle')}
      okText={t('confirm')}
      cancelText={t('cancel')}
      confirmLoading={confirming}
      okButtonProps={{ disabled: !selectedVersion || isLoading }}
      onOk={handleOk}
      onCancel={onClose}
      destroyOnHidden
    >
      <Space direction="vertical" size="middle" style={{ width: '100%', marginTop: 8 }}>
        {isBatch ? (
          <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
            {targetNames.length > 0 && targetNames.join(', ')}
          </Typography.Paragraph>
        ) : (
          currentVersion && (
            <Typography.Text type="secondary">
              {t('pages.nodes.xrayVersion')}:{' '}
              <Typography.Text strong>{currentVersion}</Typography.Text>
            </Typography.Text>
          )
        )}

        <div>
          <Typography.Paragraph strong style={{ marginBottom: 6 }}>
            {t('pages.nodes.selectXrayVersion')}
          </Typography.Paragraph>
          <Select
            style={{ width: '100%' }}
            showSearch
            loading={isLoading}
            placeholder={t('pages.nodes.selectXrayVersion')}
            value={selectedVersion || undefined}
            onChange={setUserSelectedVersion}
            options={versions.map((v) => ({ label: v, value: v }))}
          />
        </div>

        <Alert type="info" showIcon message={t('pages.nodes.adjustXrayNotice')} />
      </Space>
    </Modal>
  );
}
