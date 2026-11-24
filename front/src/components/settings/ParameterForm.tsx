import { useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";

import { useUpdateParameter, type GlobalParameter } from "@/api";
import { FormPage } from "@/components/shared/FormPage";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";

/** Edit form for one global parameter (the `settings/parameters/$key` route). */
export function ParameterForm({ parameter }: { parameter: GlobalParameter }) {
  const navigate = useNavigate();
  const [value, setValue] = useState(parameter.value);
  const goBack = () => navigate({ to: "/settings" });

  const update = useUpdateParameter({
    onSuccess: () => {
      toast.success("Parameter updated");
      goBack();
    },
    onError: (e) => toast.error(e.message),
  });

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    update.mutate({
      group: parameter.group_key,
      key: parameter.key,
      value,
    });
  };

  return (
    <FormPage
      testId="edit-parameter-page"
      title="Edit parameter"
      description={`${parameter.group_key} / ${parameter.key}`}
      backTo="/settings"
      backLabel="Settings"
      onSubmit={onSubmit}
      onCancel={goBack}
      submitLabel="Save"
      submitTestId="parameter-submit"
      submitting={update.isPending}
    >
      <div className="max-w-2xl space-y-2">
        <Label htmlFor="parameter-value">Value</Label>
        <Textarea
          id="parameter-value"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          rows={6}
          data-testid="parameter-value"
        />
      </div>
    </FormPage>
  );
}
