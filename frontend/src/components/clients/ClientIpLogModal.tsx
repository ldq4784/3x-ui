import { Button, Modal, Spin, Tag } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';
import { useTranslation } from 'react-i18next';
import type { ClientIpInfo } from '@/lib/clients/ip-log';

interface ClientIpLogModalProps {
  open: boolean;
  title: string;
  zIndex?: number;
  clientIps: ClientIpInfo[];
  ipLocations: Record<string, string>;
  loading: boolean;
  locationsLoading?: boolean;
  clearing: boolean;
  onRefresh: () => void;
  onClearAll: () => void;
  onClose: () => void;
}

// Shared IP log modal used by both ClientInfoModal and ClientFormModal.
export default function ClientIpLogModal({
  open,
  title,
  zIndex,
  clientIps,
  ipLocations,
  loading,
  locationsLoading = false,
  clearing,
  onRefresh,
  onClearAll,
  onClose,
}: ClientIpLogModalProps) {
  const { t } = useTranslation();

  return (
    <Modal
      open={open}
      title={
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
          <span>{title}</span>
          {locationsLoading ? <Spin size="small" /> : null}
        </span>
      }
      width={580}
      zIndex={zIndex}
      onCancel={onClose}
      footer={[
        <Button
          key="refresh"
          icon={<ReloadOutlined aria-hidden />}
          loading={loading}
          onClick={onRefresh}
        >
          {t('refresh')}
        </Button>,
        <Button
          key="clear"
          danger
          loading={clearing}
          disabled={clientIps.length === 0}
          onClick={onClearAll}
        >
          {t('pages.clients.clearAll')}
        </Button>,
        <Button key="close" type="primary" onClick={onClose}>
          {t('close')}
        </Button>,
      ]}
    >
      {clientIps.length > 0 ? (
        <div style={{ maxHeight: 360, overflowY: 'auto' }}>
          {clientIps.map((entry, idx) => {
            const location = entry.location || ipLocations[entry.ip] || '';
            return (
              <div
                key={`${entry.ip}-${idx}`}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  flexWrap: 'wrap',
                  gap: 6,
                  marginBottom: 6,
                }}
              >
                {location ? (
                  <Tag
                    color="cyan"
                    style={{
                      margin: 0,
                      padding: '2px 8px',
                      fontWeight: 500,
                    }}
                  >
                    {location}
                  </Tag>
                ) : null}
                <Tag
                  color="blue"
                  style={{
                    margin: 0,
                    maxWidth: '100%',
                    padding: '2px 8px',
                    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
                  }}
                >
                  {entry.ip}
                  {entry.time ? ` (${entry.time})` : ''}
                  {entry.node ? (
                    <span style={{ marginInlineStart: 6, opacity: 0.85, fontWeight: 600 }}>
                      @ {entry.node}
                    </span>
                  ) : null}
                </Tag>
              </div>
            );
          })}
        </div>
      ) : (
        <Tag>{t('tgbot.noIpRecord')}</Tag>
      )}
    </Modal>
  );
}
