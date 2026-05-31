terraform {
  # 1.3+ is required for optional() object attributes with defaults.
  # The published wrappers pin a single honest floor (see DEVELOPMENT_PLAN.md §10);
  # the core itself only needs language features available since 1.3.
  required_version = ">= 1.3.0"
}
