CREATE INDEX github_webhook_agent_issue_claim_due_idx
    ON github_webhook_deliveries (received_at, delivery_id)
    WHERE event_type = 'issue_comment' AND action = 'created'
      AND processing_status IN ('received', 'processing');
