'use client';

import { useTranslations } from 'next-intl';
import { AlertTriangle, Database, Loader2, RefreshCw } from 'lucide-react';
import { useSQLiteCheckpoint, useSQLiteStatus } from '@/api/endpoints/setting';
import { Button } from '@/components/ui/button';
import { toast } from '@/components/common/Toast';

function formatBytes(bytes?: number) {
    if (typeof bytes !== 'number' || Number.isNaN(bytes) || bytes < 0) {
        return '--';
    }
    if (bytes === 0) {
        return '0 B';
    }

    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let value = bytes;
    let unitIndex = 0;

    while (value >= 1024 && unitIndex < units.length - 1) {
        value /= 1024;
        unitIndex += 1;
    }

    const digits = value >= 100 || unitIndex === 0 ? 0 : value >= 10 ? 1 : 2;
    return `${value.toFixed(digits)} ${units[unitIndex]}`;
}

function formatCount(value?: number) {
    if (typeof value !== 'number' || Number.isNaN(value)) {
        return '--';
    }
    return new Intl.NumberFormat().format(value);
}

export function SettingSQLiteStatus() {
    const t = useTranslations('setting');
    const sqliteStatusQuery = useSQLiteStatus();
    const sqliteCheckpoint = useSQLiteCheckpoint();
    const sqliteStatus = sqliteStatusQuery.data;

    const handleSQLiteCheckpoint = () => {
        sqliteCheckpoint.mutate(undefined, {
            onSuccess: (result) => {
                if (!result.is_sqlite) {
                    toast.warning(t('info.sqlite.notSqlite'));
                    return;
                }
                toast.success(t('info.sqlite.checkpointSuccess'), {
                    description: t('info.sqlite.checkpointSuccessDetail', {
                        walSize: formatBytes(result.wal_size_bytes_after),
                        busyFrames: formatCount(result.busy_frames),
                        checkpointedFrames: formatCount(result.checkpointed_frames),
                    }),
                });
                sqliteStatusQuery.refetch();
            },
            onError: () => {
                toast.error(t('info.sqlite.checkpointFailed'));
            },
        });
    };

    const sqliteRows = sqliteStatus?.is_sqlite ? [
        { label: t('info.sqlite.journalMode'), value: sqliteStatus.journal_mode || t('info.unknown') },
        { label: t('info.sqlite.autoVacuumMode'), value: sqliteStatus.auto_vacuum_mode || t('info.unknown') },
        { label: t('info.sqlite.walAutoCheckpoint'), value: formatCount(sqliteStatus.wal_auto_checkpoint) },
        { label: t('info.sqlite.walSize'), value: formatBytes(sqliteStatus.wal_size_bytes) },
        { label: t('info.sqlite.pageCount'), value: formatCount(sqliteStatus.page_count) },
        { label: t('info.sqlite.freelistCount'), value: formatCount(sqliteStatus.freelist_count) },
        { label: t('info.sqlite.dbPath'), value: sqliteStatus.db_path || '--', mono: true },
    ] : [];

    return (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                <div>
                    <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                        <Database className="h-5 w-5" />
                        {t('info.sqlite.title')}
                    </h2>
                    <p className="mt-1 text-xs text-muted-foreground">{t('info.sqlite.description')}</p>
                </div>
                <div className="flex items-center gap-2">
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={handleSQLiteCheckpoint}
                        disabled={sqliteCheckpoint.isPending || sqliteStatusQuery.isLoading || !sqliteStatus?.is_sqlite}
                        className="rounded-xl"
                    >
                        <Database className={sqliteCheckpoint.isPending ? 'size-4 animate-pulse' : 'size-4'} />
                        {sqliteCheckpoint.isPending ? t('info.sqlite.checkpointRunning') : t('info.sqlite.checkpoint')}
                    </Button>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => sqliteStatusQuery.refetch()}
                        disabled={sqliteStatusQuery.isFetching}
                        className="rounded-xl"
                    >
                        <RefreshCw className={sqliteStatusQuery.isFetching ? 'size-4 animate-spin' : 'size-4'} />
                        {t('info.sqlite.refresh')}
                    </Button>
                </div>
            </div>

            {sqliteStatusQuery.isLoading ? (
                <div className="flex items-center gap-2 text-sm text-muted-foreground">
                    <Loader2 className="size-4 animate-spin" />
                    {t('info.sqlite.loading')}
                </div>
            ) : sqliteStatusQuery.isError ? (
                <div className="rounded-xl border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                    {t('info.sqlite.loadFailed')}
                </div>
            ) : !sqliteStatus?.is_sqlite ? (
                <div className="rounded-xl border border-border/70 bg-background/70 px-3 py-2 text-sm text-muted-foreground">
                    {t('info.sqlite.notSqlite')}
                </div>
            ) : (
                <>
                    {sqliteStatus.auto_vacuum_needs_vacuum && (
                        <div className="rounded-xl border border-amber-500/30 bg-amber-500/10 p-3 space-y-1">
                            <div className="flex items-start gap-3">
                                <AlertTriangle className="mt-0.5 h-5 w-5 shrink-0 text-amber-600" />
                                <div className="space-y-1">
                                    <p className="text-sm font-medium text-amber-700 dark:text-amber-300">
                                        {t('info.sqlite.repairRequired')}
                                    </p>
                                    <p className="text-xs text-muted-foreground">
                                        {t('info.sqlite.repairHint')}
                                    </p>
                                </div>
                            </div>
                        </div>
                    )}

                    <div className="grid gap-3 md:grid-cols-2">
                        {sqliteRows.map((row) => (
                            <div key={row.label} className="rounded-2xl border border-border/60 bg-background/70 px-3 py-3">
                                <p className="text-xs text-muted-foreground">{row.label}</p>
                                <p className={row.mono ? 'mt-1 break-all font-mono text-sm text-card-foreground' : 'mt-1 text-sm font-medium text-card-foreground'}>
                                    {row.value}
                                </p>
                            </div>
                        ))}
                    </div>
                </>
            )}
        </div>
    );
}
