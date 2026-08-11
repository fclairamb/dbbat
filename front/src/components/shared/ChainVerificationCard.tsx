import { useCallback, useState } from "react";
import { formatDistanceToNow } from "date-fns";
import { toast } from "sonner";
import {
  AlertTriangle,
  CheckCircle2,
  Copy,
  ExternalLink,
  ShieldCheck,
  XCircle,
} from "lucide-react";
import {
  useVerifyAuditChain,
  useVerifyQueryChains,
  useVerifyRowChains,
  type AuditChainVerification,
  type ChainBreak,
  type QueryChainVerification,
  type RowChainVerification,
} from "@/api";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

const DOCS_URL = "https://dbbat.com/docs/features/audit-chain";

type ChainResult =
  | AuditChainVerification
  | QueryChainVerification
  | RowChainVerification;

/**
 * Admin-only panel that walks the three HMAC chains and reports what came
 * back. Deliberately unopinionated about the answer's authority: it is served
 * by the process under audit, and it may be up to a minute old — both are
 * stated in the copy rather than glossed over.
 */
export function ChainVerificationCard() {
  const auditChain = useVerifyAuditChain();
  const queryChains = useVerifyQueryChains();
  const rowChains = useVerifyRowChains();
  const [hasRun, setHasRun] = useState(false);

  const isVerifying =
    auditChain.isPending || queryChains.isPending || rowChains.isPending;

  const handleVerify = useCallback(() => {
    setHasRun(true);
    // Independent walks: one chain failing to answer must not hide the others.
    auditChain.mutate();
    queryChains.mutate(undefined);
    rowChains.mutate(undefined);
  }, [auditChain, queryChains, rowChains]);

  return (
    <Card data-testid="chain-verification-card">
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="space-y-1.5">
            <CardTitle className="flex items-center gap-2">
              <ShieldCheck className="h-5 w-5" />
              Chain verification
            </CardTitle>
            <CardDescription data-testid="chain-verification-caveat">
              This answer comes from the server being audited and may be up to a
              minute old — <code className="font-mono">dbbat audit verify</code>{" "}
              is what someone who does not trust this server runs.{" "}
              <a
                href={DOCS_URL}
                target="_blank"
                rel="noreferrer noopener"
                className="inline-flex items-center gap-1 underline underline-offset-2"
                data-testid="chain-verification-docs-link"
              >
                Audit chain docs
                <ExternalLink className="h-3 w-3" />
              </a>
            </CardDescription>
          </div>
          <Button
            onClick={handleVerify}
            disabled={isVerifying}
            data-testid="verify-chains-button"
          >
            {isVerifying ? "Verifying…" : "Verify chains"}
          </Button>
        </div>
      </CardHeader>

      {hasRun && (
        <CardContent className="space-y-4">
          <ChainRow
            testId="chain-result-audit"
            title="Audit log"
            subtitle="One chain for the whole store"
            isPending={auditChain.isPending}
            error={auditChain.error}
            result={auditChain.data}
            renderFacts={(data) => {
              const d = data as AuditChainVerification;
              return (
                <>
                  <Fact label="Entries" value={d.entries.toLocaleString()} />
                  <Fact label="Head position" value={String(d.head_seq)} />
                  <Fact
                    label="Unverifiable (pre-anchor)"
                    value={d.unverifiable_pre_anchor_entries.toLocaleString()}
                    hint="Rows written before chaining was introduced. No MAC exists for them and none can be created after the fact."
                  />
                </>
              );
            }}
            headMac={(data) => (data as AuditChainVerification).head_mac}
            headMacTestId="audit-head-mac"
            headMacAbsentNote="No chained entries yet — the head appears once the store has recorded its first audited action."
          />

          <ChainRow
            testId="chain-result-queries"
            title="Query history"
            subtitle="One chain per connection"
            isPending={queryChains.isPending}
            error={queryChains.error}
            result={queryChains.data}
            renderFacts={(data) => {
              const d = data as QueryChainVerification;
              return (
                <>
                  <Fact
                    label="Sessions"
                    value={d.connections.toLocaleString()}
                  />
                  <Fact
                    label="Statements"
                    value={d.statements.toLocaleString()}
                  />
                  <Fact
                    label="Truncated prefixes"
                    value={d.chains_with_truncated_prefix.toLocaleString()}
                    hint="Chains missing their oldest statements — what query retention leaves behind. Expected housekeeping, not tampering."
                  />
                  <Fact
                    label="Legacy head stamps"
                    value={d.legacy_stamps.toLocaleString()}
                    warn={d.legacy_stamps > 0}
                    hint="Sessions whose head stamp predates 0.24 and is a verbatim copy of the last statement's MAC rather than a keyed seal: counted, not trusted. The number should drain as those sessions age out of retention."
                  />
                </>
              );
            }}
            headMac={(data) => (data as QueryChainVerification).head_mac}
            headMacTestId="queries-head-mac"
            headMacAbsentNote="A head is reported only for a walk scoped to one session — an aggregate head over independent chains would not mean anything."
          />

          <ChainRow
            testId="chain-result-rows"
            title="Captured result rows"
            subtitle="One chain per capture"
            isPending={rowChains.isPending}
            error={rowChains.error}
            result={rowChains.data}
            renderFacts={(data) => {
              const d = data as RowChainVerification;
              return (
                <>
                  <Fact label="Captures" value={d.captures.toLocaleString()} />
                  <Fact label="Rows" value={d.rows.toLocaleString()} />
                  <Fact
                    label="Unverifiable (pre-migration)"
                    value={d.unverifiable_pre_migration_rows.toLocaleString()}
                    hint="Rows captured before the row chain migration. No MAC exists for them."
                  />
                </>
              );
            }}
            headMac={(data) => (data as RowChainVerification).head_mac}
            headMacTestId="rows-head-mac"
            headMacAbsentNote="A head is reported only for a walk scoped to one capture."
          />
        </CardContent>
      )}
    </Card>
  );
}

interface ChainRowProps {
  testId: string;
  title: string;
  subtitle: string;
  isPending: boolean;
  error: Error | null;
  result: ChainResult | undefined;
  renderFacts: (data: ChainResult) => React.ReactNode;
  headMac: (data: ChainResult) => string | undefined;
  headMacTestId: string;
  headMacAbsentNote?: string;
}

function ChainRow({
  testId,
  title,
  subtitle,
  isPending,
  error,
  result,
  renderFacts,
  headMac,
  headMacTestId,
  headMacAbsentNote,
}: ChainRowProps) {
  const mac = result ? headMac(result) : undefined;
  const broken = result ? !result.verified : false;

  return (
    <div
      data-testid={testId}
      data-verified={result ? String(result.verified) : undefined}
      className={`rounded-lg border p-4 space-y-3 ${
        broken ? "border-destructive bg-destructive/5" : ""
      }`}
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <div className="font-medium">{title}</div>
          <div className="text-xs text-muted-foreground">{subtitle}</div>
        </div>
        <StatusBadge
          isPending={isPending}
          error={error}
          result={result}
          testId={`${testId}-status`}
        />
      </div>

      {error && (
        <Alert variant="destructive" data-testid={`${testId}-error`}>
          <XCircle className="h-4 w-4" />
          <AlertTitle>Verification could not run</AlertTitle>
          <AlertDescription>{error.message}</AlertDescription>
        </Alert>
      )}

      {result && (
        <>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            {renderFacts(result)}
          </div>

          {mac ? (
            <HeadMac value={mac} testId={headMacTestId} />
          ) : (
            headMacAbsentNote && (
              <p className="text-xs text-muted-foreground">
                {headMacAbsentNote}
              </p>
            )
          )}

          {result.break && <BreakDetail chainBreak={result.break} testId={testId} />}

          <Freshness
            cached={result.cached}
            checkedAt={result.checked_at}
            testId={`${testId}-freshness`}
          />
        </>
      )}
    </div>
  );
}

function StatusBadge({
  isPending,
  error,
  result,
  testId,
}: {
  isPending: boolean;
  error: Error | null;
  result: ChainResult | undefined;
  testId: string;
}) {
  if (isPending) {
    return (
      <Badge variant="secondary" data-testid={testId}>
        Walking…
      </Badge>
    );
  }
  if (error) {
    return (
      <Badge variant="outline" data-testid={testId}>
        Unavailable
      </Badge>
    );
  }
  if (!result) return null;
  if (!result.verified) {
    return (
      <Badge variant="destructive" data-testid={testId}>
        <AlertTriangle className="h-3 w-3 mr-1" />
        Broken
      </Badge>
    );
  }
  return (
    <Badge variant="secondary" data-testid={testId}>
      <CheckCircle2 className="h-3 w-3 mr-1" />
      Verified
    </Badge>
  );
}

function Fact({
  label,
  value,
  hint,
  warn = false,
}: {
  label: string;
  value: string;
  hint?: string;
  warn?: boolean;
}) {
  return (
    <div title={hint}>
      <div className="text-xs text-muted-foreground">{label}</div>
      <div
        className={`text-sm font-medium tabular-nums ${
          warn ? "text-amber-600 dark:text-amber-500" : ""
        }`}
      >
        {value}
      </div>
    </div>
  );
}

/**
 * The head MAC is meant to be recorded outside the database — a chain always
 * verifies against itself, so comparing today's head against the one recorded
 * last quarter is what catches a chain truncated and re-sealed with the key.
 */
function HeadMac({ value, testId }: { value: string; testId: string }) {
  const handleCopy = () => {
    navigator.clipboard.writeText(value).then(() => {
      toast.success("Head MAC copied — record it outside the database");
    });
  };

  return (
    <div className="space-y-1">
      <div className="text-xs text-muted-foreground">
        Head MAC — record it outside the database and compare it next time
      </div>
      <div className="flex items-center gap-2">
        <code
          className="flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-xs"
          data-testid={testId}
          title={value}
        >
          {value}
        </code>
        <Button
          variant="outline"
          size="icon"
          onClick={handleCopy}
          title="Copy head MAC to clipboard"
          data-testid={`${testId}-copy`}
        >
          <Copy className="h-4 w-4" />
        </Button>
      </div>
    </div>
  );
}

function BreakDetail({
  chainBreak,
  testId,
}: {
  chainBreak: ChainBreak;
  testId: string;
}) {
  return (
    <Alert variant="destructive" data-testid={`${testId}-break`}>
      <AlertTriangle className="h-4 w-4" />
      <AlertTitle>Chain broken — nothing after this point means anything</AlertTitle>
      <AlertDescription className="space-y-1">
        <div className="font-mono text-xs">chain_seq {chainBreak.chain_seq}</div>
        <div className="font-mono text-xs break-all">uid {chainBreak.uid}</div>
        {chainBreak.connection_uid && (
          <div className="font-mono text-xs break-all">
            connection_uid {chainBreak.connection_uid}
          </div>
        )}
        {chainBreak.query_uid && (
          <div className="font-mono text-xs break-all">
            query_uid {chainBreak.query_uid}
          </div>
        )}
        <div className="font-mono text-xs">{chainBreak.reason}</div>
      </AlertDescription>
    </Alert>
  );
}

/**
 * A walk's outcome is cached for a minute, so a click can legitimately return
 * a result computed 50 seconds ago. Say which one it was.
 */
function Freshness({
  cached,
  checkedAt,
  testId,
}: {
  cached: boolean;
  checkedAt: string;
  testId: string;
}) {
  const when = new Date(checkedAt);
  const ago = formatDistanceToNow(when, { addSuffix: true });

  return (
    <p
      className="text-xs text-muted-foreground"
      data-testid={testId}
      data-cached={String(cached)}
      title={when.toISOString()}
    >
      {cached
        ? `Cached result — this walk ran ${ago}, not for this request`
        : `Walked for this request ${ago}`}
    </p>
  );
}
